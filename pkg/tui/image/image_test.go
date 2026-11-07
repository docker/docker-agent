package image

import (
	"bytes"
	"container/list"
	"encoding/base64"
	stdimage "image"
	"image/color"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetInlineRegistry() {
	inlineRegistry.Lock()
	defer inlineRegistry.Unlock()
	inlineRegistry.entries = make(map[uint32]*list.Element)
	inlineRegistry.order.Init()
}

func TestMarkdownReferences(t *testing.T) {
	t.Parallel()

	refs := MarkdownReferences("before ![chart](./out/chart.png \"result\") and ![photo](<images/my photo.jpg>)\n\n`![not an image](ignored.png)`\n\n```md\n![also ignored](ignored.png)\n```")
	require.Equal(t, []MarkdownReference{
		{Alt: "chart", Source: "./out/chart.png", Start: 7, End: 41},
		{Alt: "photo", Source: "images/my photo.jpg", Start: 46, End: 77},
	}, refs)
}

func TestLoadMarkdownReferenceDataURI(t *testing.T) {
	t.Parallel()

	img := stdimage.NewRGBA(stdimage.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, img))
	ref := MarkdownReference{
		Alt:    "generated chart",
		Source: "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes()),
	}

	inline, ok := LoadMarkdownReference(t.Context(), ref)
	require.True(t, ok)
	assert.Equal(t, "generated chart", inline.Name)
	assert.Equal(t, 2, inline.Width)
	assert.Equal(t, 1, inline.Height)
	assert.NotEmpty(t, inline.PNGData)
}

func TestLoadMarkdownReferenceRejectsLocalSchemes(t *testing.T) {
	t.Parallel()

	img := stdimage.NewRGBA(stdimage.Rect(0, 0, 2, 1))
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, img))
	path := filepath.Join(t.TempDir(), "chart.png")
	require.NoError(t, os.WriteFile(path, encoded.Bytes(), 0o600))

	for _, scheme := range []string{"file", "sandbox"} {
		source := localSchemeURI(scheme, path)
		_, ok := LoadMarkdownReference(t.Context(), MarkdownReference{Alt: "chart", Source: source})
		assert.False(t, ok, source)
	}

	// Bare paths remain supported for agent-generated local images —
	// including absolute Windows paths, whose drive letter must not be
	// mistaken for a URL scheme.
	inline, ok := LoadMarkdownReference(t.Context(), MarkdownReference{Alt: "chart", Source: path})
	require.True(t, ok)
	assert.Equal(t, "chart", inline.Name)
}

// localSchemeURI builds a syntactically valid URI for path — e.g.
// file:///C:/Temp/chart.png on Windows, file:///tmp/chart.png elsewhere —
// so LoadMarkdownReference rejects it on its scheme. Concatenating
// "file://" with a backslash path would produce an unparseable URL and
// exercise the wrong code path.
func localSchemeURI(scheme, path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // file URIs address drive letters as /C:/...
	}
	u := url.URL{Scheme: scheme, Path: p}
	return u.String()
}

func TestIsWindowsDrivePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		source string
		want   bool
	}{
		{`C:\Users\me\chart.png`, true},
		{`c:/users/me/chart.png`, true},
		{`C:chart.png`, false}, // drive-relative: stays scheme-rejected
		{`file:///C:/chart.png`, false},
		{`sandbox://host/chart.png`, false},
		{`/tmp/chart.png`, false},
		{`./out/chart.png`, false},
		{``, false},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isWindowsDrivePath(tt.source))
		})
	}
}

func TestInlineRegistryIsBounded(t *testing.T) {
	resetInlineRegistry()
	defer resetInlineRegistry()

	for id := uint32(1); id <= maxInlineRegistryEntries+1; id++ {
		registerImage(id, Inline{PNGData: []byte{byte(id)}})
	}
	_, oldestPresent := registeredImage(1)
	newest, newestPresent := registeredImage(maxInlineRegistryEntries + 1)
	assert.False(t, oldestPresent)
	assert.True(t, newestPresent)
	assert.Equal(t, []byte{byte(maxInlineRegistryEntries + 1)}, newest.PNGData)
}

func TestInlineRegistryResolvesIDCollisions(t *testing.T) {
	resetInlineRegistry()
	defer resetInlineRegistry()

	firstID := registerImage(42, Inline{PNGData: []byte("first")})
	secondID := registerImage(42, Inline{PNGData: []byte("second")})
	assert.Equal(t, uint32(42), firstID)
	assert.Equal(t, uint32(43), secondID)

	first, ok := registeredImage(firstID)
	assert.True(t, ok)
	assert.Equal(t, []byte("first"), first.PNGData)
	second, ok := registeredImage(secondID)
	assert.True(t, ok)
	assert.Equal(t, []byte("second"), second.PNGData)
}

func TestRenderMarkersDisabledDoesNotReserveRows(t *testing.T) {
	SetRenderingEnabled(false)
	defer SetRenderingEnabled(true)

	markers := RenderMarkers(Inline{PNGData: []byte("png"), Width: 10, Height: 10}, 40)
	assert.Nil(t, markers)
}

func TestCellSizePreservesAspectRatioWhenHeightIsCapped(t *testing.T) {
	t.Parallel()

	cols, rows := cellSize(Inline{Width: 1000, Height: 1000}, 100)
	assert.Equal(t, 40, cols)
	assert.Equal(t, 20, rows)
}

func TestCellSizeUsesAvailableWidthForWideImage(t *testing.T) {
	t.Parallel()

	cols, rows := cellSize(Inline{Width: 1600, Height: 900}, 100)
	assert.Equal(t, 60, cols)
	assert.Equal(t, 17, rows)
}

func TestKittySequenceChunksLargePayload(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("x"), 4096)

	seq := KittySequence(data, 10, 5)
	encoded := base64.StdEncoding.EncodeToString(data)

	assert.Contains(t, seq, "m=1")
	assert.Contains(t, seq, "\x1b_Gm=0;")
	assert.Contains(t, seq, encoded[:100])
}
