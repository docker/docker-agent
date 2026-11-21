package image

import (
	"bytes"
	"errors"
	stdimage "image"
	"image/png"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type failSecondWrite struct {
	buf    bytes.Buffer
	writes int
	failed bool
}

func (w *failSecondWrite) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == 2 && !w.failed {
		w.failed = true
		return 0, errors.New("terminal write failed")
	}
	return w.buf.Write(p)
}

func (w *failSecondWrite) Reset() {
	w.buf.Reset()
}

func (w *failSecondWrite) String() string {
	return w.buf.String()
}

func TestWriterRetriesImageStateAfterOverlayWriteFailure(t *testing.T) {
	output := &failSecondWrite{}
	writer := NewWriter(output)
	writer.SetContent(KittySequence([]byte("png-data"), 20, 10))

	_, err := writer.Write([]byte("first frame"))
	require.Error(t, err)
	assert.Empty(t, writer.uploaded)
	assert.True(t, writer.dirty)
	assert.False(t, writer.active)

	output.Reset()
	_, err = writer.Write([]byte("second frame"))
	require.NoError(t, err)
	assert.Contains(t, output.String(), "a=t,t=d", "failed image upload must be retried")
	assert.Contains(t, output.String(), "a=d,d=a", "failed placement reset must be retried")
}

func TestWriterExtractsAndOverlaysKittyImage(t *testing.T) {
	data := []byte("png-data")
	content := "header\n    " + KittySequence(data, 20, 10) + "\n"
	var output bytes.Buffer
	writer := NewWriter(&output)

	clean := writer.SetContent(content)
	require.NotContains(t, clean, "\x1b_G")
	assert.Equal(t, "header\n    \n", clean)

	_, err := writer.Write([]byte("frame"))
	require.NoError(t, err)
	rendered := output.String()
	assert.True(t, strings.HasPrefix(rendered, "frame"))
	assert.Contains(t, rendered, "a=t,t=d,f=100")
	assert.Contains(t, rendered, "\x1b[2;5H")
	assert.Contains(t, rendered, "a=p,i=")
	assert.Contains(t, rendered, "c=20,r=10")
}

func TestWriterOnlyUploadsUnchangedImageOnce(t *testing.T) {
	var output bytes.Buffer
	writer := NewWriter(&output)
	writer.SetContent(KittySequence([]byte("png-data"), 20, 10))
	_, err := writer.Write([]byte("first"))
	require.NoError(t, err)

	output.Reset()
	writer.SetContent(KittySequence([]byte("png-data"), 20, 10))
	_, err = writer.Write([]byte("second"))
	require.NoError(t, err)
	assert.NotContains(t, output.String(), "a=t,t=d", "image data should remain cached")
	assert.Contains(t, output.String(), "a=p", "placement is refreshed after terminal text updates")
}

func TestMarkerOverlayCropsScrolledImage(t *testing.T) {
	img := Inline{Name: "test", PNGData: []byte("png-data"), Width: 100, Height: 100}
	lines := RenderMarkers(img, 24)
	require.GreaterOrEqual(t, len(lines), 8)

	clean, overlays := extractOverlays(strings.Join(lines[4:8], "\n"))
	require.Len(t, overlays, 1)
	assert.NotContains(t, clean, markerPrefix)
	assert.Equal(t, 0, overlays[0].y)
	assert.Equal(t, 4, overlays[0].rows)
	assert.Positive(t, overlays[0].sourceY)
	assert.Less(t, overlays[0].sourceH, img.Height)
}

func TestWriterClipsSingleVisibleUpscaledMarkerRow(t *testing.T) {
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, stdimage.NewGray(stdimage.Rect(0, 0, 16, 16))))
	img := Inline{PNGData: data.Bytes(), Width: 16, Height: 16}
	lines := RenderMarkers(img, 44)
	require.Len(t, lines, 20)

	var output bytes.Buffer
	writer := NewWriter(&output)
	writeFrame := func() string {
		t.Helper()
		output.Reset()
		_, err := writer.Write([]byte("frame"))
		require.NoError(t, err)
		return output.String()
	}

	writer.SetContent(strings.Join(lines, "\n"))
	commands := writeFrame()
	require.Equal(t, 1, strings.Count(commands, "\x1b_Ga=p,"), "contiguous marker rows remain grouped")
	assert.Contains(t, commands, ",c=40,r=20\x1b\\")

	writer.SetContent(lines[0])
	writer.SetOcclusion(stdimage.Rect(12, 0, 13, 1))
	commands = writeFrame()
	require.Equal(t, 2, strings.Count(commands, "\x1b_Ga=p,"), commands)
	assert.Contains(t, commands, ",c=10,r=1,x=0,y=0,w=4,h=1\x1b\\")
	assert.Contains(t, commands, ",c=29,r=1,x=4,y=0,w=12,h=1\x1b\\")
	assert.Contains(t, commands, "\x1b[1;14H\x1b_Ga=p,")
	assert.NotContains(t, commands, "\x1b_Ga=t,")

	writer.SetOcclusion(stdimage.Rect(2, 0, 42, 1))
	commands = writeFrame()
	assert.Contains(t, commands, "\x1b_Ga=d,")
	assert.NotContains(t, commands, "\x1b_Ga=p,")

	writer.SetOcclusion(stdimage.Rectangle{})
	commands = writeFrame()
	require.Equal(t, 1, strings.Count(commands, "\x1b_Ga=p,"))
	assert.Contains(t, commands, "\x1b[1;3H\x1b_Ga=p,")
	assert.Contains(t, commands, ",c=40,r=1,")
	assert.NotContains(t, commands, "\x1b_Ga=t,")

	// A foreground mask at the image edge must retain the later row's source offset.
	writer.SetContent(lines[15], stdimage.Rect(2, 0, 3, 1))
	commands = writeFrame()
	require.Equal(t, 1, strings.Count(commands, "\x1b_Ga=p,"))
	assert.Contains(t, commands, "\x1b[1;4H\x1b_Ga=p,")
	assert.Contains(t, commands, ",c=39,r=1,x=0,y=12,w=16,h=1\x1b\\")
}

func TestWriterUnsupportedTerminalStripsMarkersWithoutRendering(t *testing.T) {
	img := Inline{PNGData: []byte("png-data"), Width: 100, Height: 100}
	var output bytes.Buffer
	writer := NewWriter(&output)
	writer.SetSupported(false)

	clean := writer.SetContent(strings.Join(RenderMarkers(img, 40), "\n"))
	assert.NotContains(t, clean, markerPrefix)
	_, err := writer.Write([]byte("frame"))
	require.NoError(t, err)
	assert.Equal(t, "frame", output.String())
}

func TestWriterDisabledStripsMarkersWithoutRendering(t *testing.T) {
	img := Inline{PNGData: []byte("png-data"), Width: 100, Height: 100}
	var output bytes.Buffer
	writer := NewWriter(&output)
	writer.SetEnabled(false)

	clean := writer.SetContent(strings.Join(RenderMarkers(img, 40), "\n"))
	assert.NotContains(t, clean, markerPrefix)
	_, err := writer.Write([]byte("frame"))
	require.NoError(t, err)
	assert.Equal(t, "frame", output.String())
}

func TestWriterDeletesPlacementWhenImageLeavesView(t *testing.T) {
	var output bytes.Buffer
	writer := NewWriter(&output)
	writer.SetContent(KittySequence([]byte("png-data"), 20, 10))
	_, err := writer.Write([]byte("first"))
	require.NoError(t, err)

	output.Reset()
	writer.SetContent("no image")
	_, err = writer.Write([]byte("second"))
	require.NoError(t, err)
	assert.Contains(t, output.String(), "a=d,d=a")
}

func TestWriterDisablingClearsExistingPlacements(t *testing.T) {
	for _, capability := range []bool{false, true} {
		t.Run(map[bool]string{false: "preference", true: "capability"}[capability], func(t *testing.T) {
			var output bytes.Buffer
			writer := NewWriter(&output)
			content := KittySequence([]byte("png-data"), 20, 10)
			writer.SetContent(content)
			_, err := writer.Write([]byte("first frame"))
			require.NoError(t, err)
			output.Reset()
			if capability {
				writer.SetSupported(false)
			} else {
				writer.SetEnabled(false)
			}
			// A hidden pane can retain its previous frame without calling SetContent.
			_, err = writer.Write([]byte("cached frame"))
			require.NoError(t, err)
			assert.Contains(t, output.String(), "a=d,d=a")
			assert.NotContains(t, output.String(), "a=p,i=")
			assert.NotContains(t, output.String(), "a=t,t=d")
			output.Reset()
			writer.SetSupported(true)
			writer.SetEnabled(true)
			writer.SetContent(content)
			_, err = writer.Write([]byte("enabled frame"))
			require.NoError(t, err)
			assert.Contains(t, output.String(), "a=p,i=")
		})
	}
}
