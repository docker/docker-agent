package ui

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
)

func TestRenderInlineImageIncludesKittyImageSequence(t *testing.T) {
	t.Parallel()
	img := InlineImage{
		Name:    "sample.png",
		MIME:    "image/png",
		PNGData: testPNGData(t),
		Width:   2,
		Height:  1,
	}

	lines := RenderInlineImage(img, 80)
	joined := strings.Join(lines, "\n")

	assert.Contains(t, joined, "\x1b_G")
	assert.Contains(t, joined, "a=T")
	assert.Contains(t, joined, "f=100")
	require.Len(t, lines, 15)
	assert.Equal(t, "  "+tuiimage.KittySequence(img.PNGData, 60, 15), lines[0])
	for _, line := range lines[1:] {
		assert.Empty(t, line, "remaining rows must only reserve space for the image")
	}
}

func testPNGData(t *testing.T) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 0, color.RGBA{B: 255, A: 255})

	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}
