package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dgageot/rubocop-go/coptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSilentLimitedRead(t *testing.T) {
	t.Parallel()
	const read = `body, err := stream.ReadAll(stream.LimitReader(resp.Body, 1<<20)); if err != nil { return err }; `
	const write = `return disk.WriteFile("cache", body, 0600)`
	tests := []struct {
		name, body string
		want       int
	}{
		{"cache file", read + write, 1},
		{"parenthesized read call", `body, _ := (stream.ReadAll)(stream.LimitReader(resp.Body, limit)); ` + write, 1},
		{"no read call", `return disk.WriteFile("cache", otherData, 0600)`, 0},
		{"nested closure read only", `_ = func() error { ` + read + write + ` }; return nil`, 1},
		{"write in if initializer", read + `if err := disk.WriteFile("cache", body, 0600); err != nil { return err }; return nil`, 1},
		{"parenthesized", `body, _ := stream.ReadAll((stream.LimitReader(resp.Body, 1<<20))); return disk.WriteFile("cache", (body), 0600)`, 1},
		{"var declaration", `var body, err = stream.ReadAll(stream.LimitReader(resp.Body, 1<<20)); if err != nil { return err }; ` + write, 1},
		{"local reader", `r := stream.LimitReader(resp.Body, 1<<20); body, err := stream.ReadAll(r); if err != nil { return err }; ` + write, 1},
		{"local reader alias", `r := stream.LimitReader(resp.Body, 1<<20); alias := r; body, _ := stream.ReadAll(alias); ` + write, 1},
		{"runtime bound", `body, _ := stream.ReadAll(stream.LimitReader(resp.Body, limit)); ` + write, 1},
		{"json scalar outside scope", read + `var value int; return json.Unmarshal(body, &value)`, 0},
		{"json object is not a file truncation", read + `var value struct { Skills []string }; return json.Unmarshal(body, &value)`, 0},
		{"json interface outside scope", read + `var value any; return json.Unmarshal(body, &value)`, 0},
		{"unrelated len", read + `_ = len(body); ` + write, 1},
		{"unrelated len condition", read + `if len(body) == 0 { return nil }; ` + write, 1},
		{"unrelated data guard", read + `if len(otherData) >= 1<<20 { return nil }; ` + write, 1},
		{"too weak guard", read + `if len(body) > 1<<20 { return nil }; ` + write, 1},
		{"logged overflow still written", read + `if len(body) >= 1<<20 { fmt.Println("large") }; ` + write, 1},
		{"conditional rejection", read + `if len(body) >= 1<<20 { if enabled { return nil } }; ` + write, 1},
		{"guard after sink", read + `_ = disk.WriteFile("cache", body, 0600); if len(body) >= 1<<20 { return nil }; return nil`, 1},
		{"guard sometimes bypassed", read + `if enabled { if len(body) >= 1<<20 { return nil } }; ` + write, 1},
		{"short circuit allows overflow", read + `if enabled && len(body) >= 1<<20 { return nil }; ` + write, 1},
		{"write inside overflow arm", read + `if len(body) >= 1<<20 { return disk.WriteFile("cache", body, 0600) }; return nil`, 1},
		{"shadowed body guard", read + `{ body := otherData; if len(body) >= 1<<20 { return nil } }; ` + write, 1},
		{"shadowed len", read + `len := func([]byte) int { return 0 }; if len(body) >= 1<<20 { return nil }; ` + write, 0},
		{"preview", read + `fmt.Println(string(body)); return nil`, 0},
		{"error excerpt", read + `return fmt.Errorf("HTTP error: %s", body)`, 0},
		{"complete read", `body, _ := stream.ReadAll(resp.Body); ` + write, 0},
		{"zero limit", `body, _ := stream.ReadAll(stream.LimitReader(resp.Body, 0)); ` + write, 0},
		{"negative limit", `body, _ := stream.ReadAll(stream.LimitReader(resp.Body, -1)); ` + write, 0},
		{"overflow rejection", `body, _ := stream.ReadAll(stream.LimitReader(resp.Body, (1<<20)+1)); if len(body) > 1<<20 { return nil }; ` + write, 0},
		{"extra byte without check", `body, _ := stream.ReadAll(stream.LimitReader(resp.Body, (1<<20)+1)); ` + write, 1},
		{"overflow converted length", `body, _ := stream.ReadAll(stream.LimitReader(resp.Body, (1<<20)+1)); if int64(len(body)) > 1<<20 { return nil }; ` + write, 0},
		{"unrelated runtime bound", `body, _ := stream.ReadAll(stream.LimitReader(resp.Body, limit)); if len(body) >= len(otherData) { return nil }; ` + write, 1},
		{"overflow equals rejection", `body, _ := stream.ReadAll(stream.LimitReader(resp.Body, (1<<20)+1)); if len(body) == (1<<20)+1 { return nil }; ` + write, 0},
		{"at bound rejection", read + `if len(body) >= 1<<20 { return nil }; ` + write, 0},
		{"inverted rejection", read + `if 1<<20 <= len(body) { return nil }; ` + write, 0},
		{"safe branch only", read + `if len(body) < 1<<20 { return disk.WriteFile("cache", body, 0600) }; return nil`, 0},
		{"else safe branch", read + `if len(body) >= 1<<20 { return nil } else { return disk.WriteFile("cache", body, 0600) }`, 0},
		{"short circuit rejection", read + `if enabled || len(body) >= 1<<20 { return nil }; ` + write, 0},
		{"short circuit safe write", read + `if len(body) < 1<<20 && disk.WriteFile("cache", body, 0600) != nil { return nil }; return nil`, 0},
		{"short circuit safe or write", read + `if len(body) >= 1<<20 || disk.WriteFile("cache", body, 0600) != nil { return nil }; return nil`, 0},
		{"nested short circuit safe write", read + `if enabled && (len(body) < 1<<20 && disk.WriteFile("cache", body, 0600) != nil) { return nil }; return nil`, 0},
		{"short circuit safe assignment", read + `ok := len(body) < 1<<20 && disk.WriteFile("cache", body, 0600) == nil; _ = ok; return nil`, 0},
		{"short circuit safe json", read + `var value any; if len(body) < 1<<20 && json.Unmarshal(body, &value) != nil { return nil }; return nil`, 0},
		{"short circuit left sink still checked", read + `if disk.WriteFile("cache", body, 0600) != nil && enabled { return nil }; return nil`, 1},
		{"short circuit sink then unguarded sink", read + `if len(body) < 1<<20 && disk.WriteFile("first", body, 0600) != nil { return nil }; ` + write, 1},
		// The CFG does not model conditional expression evaluation; skip right-hand sinks.
		{"short circuit unguarded right sink skipped", read + `if enabled && disk.WriteFile("cache", body, 0600) != nil { return nil }; return nil`, 0},
		{"negated safe length", read + `if !(len(body) < 1<<20) { return nil }; ` + write, 0},
		{"panic rejects overflow", read + `if len(body) >= 1<<20 { panic("too big") }; ` + write, 0},
		{"runtime overflow", `body, _ := stream.ReadAll(stream.LimitReader(resp.Body, limit+1)); if int64(len(body)) > limit { return nil }; ` + write, 0},
		{"runtime weak overflow", `body, _ := stream.ReadAll(stream.LimitReader(resp.Body, limit)); if int64(len(body)) > limit { return nil }; ` + write, 1},
		{"runtime bound changed", `body, _ := stream.ReadAll(stream.LimitReader(resp.Body, limit)); limit++; ` + write, 0},
		{"range changes bound", `limit := int64(4); body, _ := stream.ReadAll(stream.LimitReader(resp.Body, limit)); for _, limit = range []int64{100} {}; if int64(len(body)) >= limit { return nil }; ` + write, 0},
		{"range decreases bound", `limit := int64(100); body, _ := stream.ReadAll(stream.LimitReader(resp.Body, limit)); for _, limit = range []int64{4} {}; if int64(len(body)) > limit { return nil }; ` + write, 0},
		{"parenthesized bound escapes", `body, _ := stream.ReadAll(stream.LimitReader(resp.Body, limit)); mutateLimit(&((limit))); if int64(len(body)) >= limit { return nil }; ` + write, 0},
		{"constructed response length", `response := &http.Response{ContentLength: 1, Body: stream.NopCloser(strings.NewReader("toolong"))}; if response.ContentLength < 0 || response.ContentLength > 4 { return nil }; body, _ := stream.ReadAll(stream.LimitReader(response.Body, 4)); ` + write, 1},
		{"constructed response with overflow check", `response := &http.Response{ContentLength: 1, Body: stream.NopCloser(strings.NewReader("toolong"))}; body, _ := stream.ReadAll(stream.LimitReader(response.Body, 5)); if len(body) > 4 { return nil }; ` + write, 0},

		{"parenthesized bound assignment", `limit := int64(100); body, _ := stream.ReadAll(stream.LimitReader(resp.Body, limit)); (limit) = 4; if int64(len(body)) > limit { return nil }; ` + write, 0},
		{"parenthesized bound decrement", `limit := int64(100); body, _ := stream.ReadAll(stream.LimitReader(resp.Body, limit)); (limit)--; if int64(len(body)) > limit { return nil }; ` + write, 0},
		{"parenthesized range assignment", `limit := int64(100); body, _ := stream.ReadAll(stream.LimitReader(resp.Body, limit)); for _, (limit) = range []int64{4} {}; if int64(len(body)) > limit { return nil }; ` + write, 0},
		{"stable threshold alias", `limit := int64(disk.Getpid()); threshold := limit; body, _ := stream.ReadAll(stream.LimitReader(resp.Body, limit)); if int64(len(body)) >= threshold { return nil }; ` + write, 0},
		{"constant bound conversion", `limit := int64(100); body, _ := stream.ReadAll(stream.LimitReader(resp.Body, limit)); if len(body) >= int(limit) { return nil }; ` + write, 0},
		{"stable limit offset", `maximum := int64(disk.Getpid()); limit := maximum + 1; body, _ := stream.ReadAll(stream.LimitReader(resp.Body, limit)); if int64(len(body)) > maximum { return nil }; ` + write, 0},

		{"narrow integer arithmetic wraps", `limit := uint8(255); readLimit := limit + 2; body, _ := stream.ReadAll(stream.LimitReader(resp.Body, int64(readLimit))); if len(body) > int(limit) { return nil }; ` + write, 1},

		{"unreachable sink", read + `return nil; _ = disk.WriteFile("cache", body, 0600)`, 0},
		{"transformed body", read + `body = bytes.TrimSpace(body); ` + write, 0},
		{"mutation", read + `body[0] = 'x'; ` + write, 0},
		{"slice", read + `return disk.WriteFile("cache", body[:10], 0600)`, 0},
		{"body alias", read + `alias := body; return disk.WriteFile("cache", alias, 0600)`, 0},
		{"body escapes", read + `consume(body); ` + write, 0},
		{"body address escapes", read + `mutate(&body); ` + write, 0},
		{"captured body", read + `_ = func(){ consume(body) }; ` + write, 0},
		{"inside function literal", `return func() error { ` + read + write + ` }()`, 1},
		{"reader changed", `r := stream.LimitReader(resp.Body, 1<<20); r = other.Body; body, _ := stream.ReadAll(r); ` + write, 0},
		{"reader escapes", `r := stream.LimitReader(resp.Body, 1<<20); consume(r); body, _ := stream.ReadAll(r); ` + write, 0},
		{"shadowed readall", `stream := fakeIO{}; body, _ := stream.ReadAll(stream.LimitReader(resp.Body, 1<<20)); ` + write, 0},
		{"shadowed limitreader", `stream := fakeIO{}; body, _ := readAll(stream.LimitReader(resp.Body, 1<<20)); ` + write, 0},
		{"shadowed writer", read + `disk := fakeDisk{}; return disk.WriteFile("cache", body, 0600)`, 0},
		{"shadowed json", read + `json := fakeJSON{}; var value any; return json.Unmarshal(body, &value)`, 0},
		{"content length does not prove framing", `if resp.ContentLength < 0 || resp.ContentLength > 1<<20 { return nil }; ` + read + write, 1},
		{"content length after read does not prove framing", read + `if resp.ContentLength < 0 || resp.ContentLength > 1<<20 { return nil }; ` + write, 1},
		{"unknown length not rejected", `if resp.ContentLength > 1<<20 { return nil }; ` + read + write, 1},
		{"unrelated response length", `if other.ContentLength < 0 || other.ContentLength > 1<<20 { return nil }; ` + read + write, 1},
		{"response body replaced", `if resp.ContentLength < 0 || resp.ContentLength > 1<<20 { return nil }; resp.Body = other.Body; ` + read + write, 1},
		{"response escapes", `if resp.ContentLength < 0 || resp.ContentLength > 1<<20 { return nil }; consume(resp); ` + read + write, 1},
		{"request length is only a hint", `if request.ContentLength < 0 || request.ContentLength > 1<<20 { return nil }; body, _ := stream.ReadAll(stream.LimitReader(request.Body, 1<<20)); ` + write, 1},
		{"file stat cannot bound a later read", `info, err := file.Stat(); if err != nil { return err }; if info.Size() > 1<<20 { return nil }; body, _ := stream.ReadAll(stream.LimitReader(file, 1<<20)); ` + write, 1},
		{"stat another file", `otherFile, err := disk.Open("other"); if err != nil { return err }; defer otherFile.Close(); info, _ := otherFile.Stat(); if info.Size() > 1<<20 { return nil }; body, _ := stream.ReadAll(stream.LimitReader(file, 1<<20)); ` + write, 1},
		{"file modified after stat", `info, err := file.Stat(); if err != nil { return err }; if info.Size() > 1<<20 { return nil }; _ = file.Truncate(2<<20); body, _ := stream.ReadAll(stream.LimitReader(file, 1<<20)); ` + write, 1},
		{"two sinks one unguarded", read + `if len(body) < 1<<20 { _ = disk.WriteFile("first", body, 0600) }; ` + write, 1},
	}
	files := coptest.ProgramFiles{"go.mod": "module fixture\n\ngo 1.27\n"}
	for i, tc := range tests {
		files[fmt.Sprintf("case%d/sample.go", i)] = limitedReadFixture + `
func run(resp, other *http.Response, request *http.Request, file *disk.File, limit int64, enabled bool) error { ` + tc.body + ` }
`
	}
	got := make(map[string]int)
	for _, offense := range coptest.RunProgram(t, SilentLimitedRead, files) {
		assert.Equal(t, "Lint/SilentLimitedRead", offense.CopName)
		assert.Contains(t, offense.Message, "silently truncate")
		got[filepath.Base(filepath.Dir(offense.Pos.Filename))]++
	}
	for i, tc := range tests {
		assert.Equal(t, tc.want, got[fmt.Sprintf("case%d", i)], tc.name)
	}
}

func TestSilentLimitedReadFileCanGrowAfterStat(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "growing")
	require.NoError(t, os.WriteFile(path, []byte("a"), 0o600))
	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()
	info, err := file.Stat()
	require.NoError(t, err)
	const limit = 4
	require.LessOrEqual(t, info.Size(), int64(limit))

	// Another writer can grow the file after Stat without changing its snapshot.
	require.NoError(t, os.WriteFile(path, []byte("larger"), 0o600))
	body, err := io.ReadAll(io.LimitReader(file, limit))
	require.NoError(t, err)
	assert.Equal(t, "larg", string(body))
	assert.Equal(t, int64(1), info.Size())
}

func TestSilentLimitedReadResponseBodyHonorsContentLength(t *testing.T) {
	t.Parallel()
	const response = "HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\nabcEXTRA"
	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(response)), nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	// HTTP framing bounds the body even when more bytes follow on the connection.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4))
	require.NoError(t, err)
	assert.Equal(t, "abc", string(body))
	assert.Equal(t, resp.ContentLength, int64(len(body)))
}

const limitedReadFixture = `package fixture
import (
 "bytes"
 "encoding/json"
 "fmt"
 stream "io"
 "net/http"
 "strings"
 disk "os"
)
var _ = strings.NewReader
var _ = bytes.TrimSpace
var _ = json.Unmarshal
var _ = fmt.Errorf
var otherData = []byte("other")
var readAll = stream.ReadAll
func consume(any) {}
func mutate(*[]byte) {}
func mutateLimit(*int64) {}
type fakeIO struct{}
func (fakeIO) ReadAll(stream.Reader) ([]byte, error) { return nil, nil }
func (fakeIO) LimitReader(stream.Reader, int64) stream.Reader { return nil }
type fakeDisk struct{}
func (fakeDisk) WriteFile(string, []byte, disk.FileMode) error { return nil }
type fakeJSON struct{}
func (fakeJSON) Unmarshal([]byte, any) error { return nil }
`

func TestSilentLimitedReadScope(t *testing.T) {
	t.Parallel()
	const source = `package fixture
import ("io"; "os")
func run(r io.Reader) error {
 body, _ := io.ReadAll(io.LimitReader(r, 1024))
 return os.WriteFile("cache", body, 0600)
}
`
	offenses := coptest.RunProgram(t, SilentLimitedRead, coptest.ProgramFiles{
		"pkg/config/v0/sample.go":     source,
		"pkg/config/v12/sample.go":    source,
		"pkg/config/latest/sample.go": source,
		"generated/sample.go":         "// Code generated by fixture; DO NOT EDIT.\n" + source,
		"tests/sample.go":             "package fixture\n",
		"tests/sample_test.go":        source,
	})
	require.Len(t, offenses, 1)
	assert.Contains(t, filepath.ToSlash(offenses[0].Pos.Filename), "/pkg/config/latest/")
}

func TestSilentLimitedReadFetchAndStore(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("../pkg/skills/cache.go")
	require.NoError(t, err)
	offenses := coptest.RunProgram(t, SilentLimitedRead, coptest.ProgramFiles{
		"go.mod":              "module github.com/docker/docker-agent\n\ngo 1.27\n",
		"pkg/skills/cache.go": string(src),
		"pkg/httpclient/client.go": `package httpclient
import ("net/http"; "time")
func NewSafeClient(time.Duration, bool) *http.Client { return http.DefaultClient }
`,
		"pkg/atomicfile/write.go": `package atomicfile
import ("io"; "os")
func Write(string, io.Reader, os.FileMode) error { return nil }
`,
	})
	assert.Empty(t, offenses)
}

func TestSilentLimitedReadSuppressions(t *testing.T) {
	testCorrectnessCopSuppressions(t, SilentLimitedRead, `package sample
import ("io"; "os")
func f(r io.Reader) error { body, _ := io.ReadAll(io.LimitReader(r, 1024)); return os.WriteFile("cache", body, 0600) } // offense
`)
}
