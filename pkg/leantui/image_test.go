package leantui

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/tools"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
)

func TestInlineImagesFromToolResultIncludesImagesAndImageDocuments(t *testing.T) {
	t.Parallel()
	b64 := testPNGBase64(t)
	result := &tools.ToolCallResult{
		Images: []tools.MediaContent{{Data: b64, MimeType: "image/png"}},
		Documents: []tools.DocumentContent{
			{Name: "screenshot.png", MimeType: "image/png", Data: b64},
			{Name: "report.pdf", MimeType: "application/pdf", Data: b64},
		},
	}

	images := tuiimage.FromToolResult(result)

	require.Len(t, images, 2)
	assert.Equal(t, "image-1", images[0].Name)
	assert.Equal(t, "screenshot.png", images[1].Name)
	assert.Equal(t, "image/png", images[0].MIME)
	assert.NotEmpty(t, images[0].PNGData)
}

func TestRenderToolIncludesInlineImage(t *testing.T) {
	t.Parallel()
	b64 := testPNGBase64(t)
	images := tuiimage.FromToolResult(&tools.ToolCallResult{
		Images: []tools.MediaContent{{Data: b64, MimeType: "image/png"}},
	})
	require.Len(t, images, 1)

	tv := ui.NewToolView("root", tools.ToolCall{
		ID: "call-1",
		Function: tools.FunctionCall{
			Name:      "image_tool",
			Arguments: `{"file":"sample.png"}`,
		},
	}, tools.Tool{Name: "image_tool"}, tuitypes.ToolStatusCompleted)
	tv.Message().Content = "Read image file sample.png"
	tv.SetImages(images)

	lines := ui.RenderTool(*tv, 80)
	joined := strings.Join(lines, "\n")

	assert.Contains(t, joined, "Read image file sample.png")
	assert.Contains(t, joined, "\x1b_G")
	assert.Contains(t, joined, "🖼")
}

func TestInlineImageFromBase64RejectsNonImages(t *testing.T) {
	t.Parallel()
	_, ok := tuiimage.FromBase64("notes.txt", "text/plain", base64.StdEncoding.EncodeToString([]byte("hello")))
	assert.False(t, ok)
}

func testPNGBase64(t *testing.T) string {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 0, color.RGBA{B: 255, A: 255})

	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
