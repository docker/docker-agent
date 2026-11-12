package programstatus

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSequence(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "\x1b]7501;state=working:app=docker-agent\x1b\\", (Status{State: "working"}).Sequence())
	assert.Equal(t, "\x1b]7501;state=blocked:app=docker-agent:kind=auth:msg=TG9naW4=\x1b\\", (Status{State: "blocked", Kind: "auth", Msg: "Login"}).Sequence())
	assert.NotContains(t, (Status{State: "done", Kind: "auth"}).Sequence(), "kind=")

	seq := (Status{State: "blocked", Kind: "question", Msg: "\x1b[31mHello\x1b[0m\nworld\u0080" + strings.Repeat("界", 1000)}).Sequence()
	require.LessOrEqual(t, len(seq), 4096)
	_, encoded, ok := strings.Cut(strings.TrimSuffix(seq, "\x1b\\"), ":msg=")
	require.True(t, ok)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(decoded), 2048)
	assert.True(t, utf8.Valid(decoded))
	assert.True(t, strings.HasPrefix(string(decoded), "Hello world "))
	for _, r := range string(decoded) {
		assert.False(t, unicode.IsControl(r))
	}
}

func TestAggregate(t *testing.T) {
	t.Parallel()
	states := []Status{{State: "idle"}, {State: "done"}, {State: "error"}, {State: "working"}, {State: "blocked", Kind: "permission", Msg: "Approve"}}
	assert.Equal(t, Status{State: "idle"}, Aggregate(nil))
	for i, want := range states {
		assert.Equal(t, want, Aggregate(states[:i+1]))
	}
	assert.Equal(t, states[4], Aggregate(append(states, Status{State: "blocked", Kind: "auth"})))
}

func TestReporter(t *testing.T) {
	t.Parallel()
	r := &Reporter{}
	assert.True(t, r.Set(Status{State: "working"}))
	assert.False(t, r.Set(Status{State: "working"}))
	r.Set(Status{State: "done"})
	assert.Contains(t, r.String(), "state=done", "queued writes use the current state")
	assert.Empty(t, r.String(), "coalesce duplicate writes")
	for _, state := range []string{"idle", "working", "blocked", "done", "error"} {
		r := &Reporter{}
		r.Set(Status{State: state})
		var buf bytes.Buffer
		r.Finish(&buf)
		want := "idle"
		if state == "done" || state == "error" {
			want = state
		}
		assert.Contains(t, buf.String(), "state="+want)
		assert.NotContains(t, buf.String(), "state=clear", "do not clear records owned by tools")
	}
}
