package editor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadAttachmentPreview_SVGShowsTextAndUnsupportedBinaryFailsGracefully(t *testing.T) {
	dir := t.TempDir()

	svgPath := filepath.Join(dir, "diagram.svg")
	svgContent := `<svg xmlns="http://www.w3.org/2000/svg"><circle r="5"/></svg>`
	require.NoError(t, os.WriteFile(svgPath, []byte(svgContent), 0o600))

	svgPreview := loadAttachmentPreview("diagram.svg", svgPath)
	assert.False(t, svgPreview.IsImage, "SVG should fall through to text preview")
	assert.Equal(t, svgContent, svgPreview.Content)

	// Fake binary .webp bytes (not decodable by stdlib png/gif decoders)
	webpPath := filepath.Join(dir, "photo.webp")
	require.NoError(t, os.WriteFile(webpPath, []byte{0x52, 0x49, 0x46, 0x46, 0x00, 0x00, 0x00, 0x00, 0x57, 0x45, 0x42, 0x50}, 0o600))

	webpPreview := loadAttachmentPreview("photo.webp", webpPath)
	assert.False(t, webpPreview.IsImage, "undecodable image format should not claim raster preview")
	assert.Contains(t, webpPreview.Content, "Attachment preview unavailable")
}
