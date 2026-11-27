package dialog

import (
	"bytes"
	"fmt"
	"image"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const AttachmentPreviewLayerID = "attachment-preview"

type attachmentPreviewDialog struct{ readOnlyScrollDialog }

// NewAttachmentPreviewDialog shows text or a bounded preview through the existing
// image renderer. It never interprets attachment bytes as terminal controls.
func NewAttachmentPreviewDialog(preview editor.AttachmentPreview, renderImages bool) Dialog {
	var picture tuiimage.Inline
	if preview.IsImage {
		const maxPixels = 16_000_000
		config, format, err := image.DecodeConfig(bytes.NewReader(preview.ImageData))
		switch {
		case err != nil:
			preview.Content = "Image preview unavailable: invalid or unsupported image data."
		case config.Width <= 0 || config.Height <= 0 || config.Width > maxPixels/config.Height:
			preview.Content = "Image preview unavailable: image exceeds the 16 megapixel preview limit."
		default:
			preview.Content = fmt.Sprintf("image/%s · %d × %d pixels", format, config.Width, config.Height)
			if renderImages {
				var ok bool
				picture, ok = tuiimage.FromBytes(preview.Title, "image/"+format, preview.ImageData)
				if !ok {
					preview.Content += "\n\nImage preview unavailable: unable to decode image data."
				}
			} else {
				preview.Content += "\n\nImage rendering is disabled or unsupported in this terminal."
			}
		}
	}
	var cached []string
	var cachedWidth int
	var cachedTheme *styles.Theme
	d := &attachmentPreviewDialog{}
	d.readOnlyScrollDialog = newReadOnlyScrollDialog(readOnlyScrollDialogSize{
		widthPercent: 80, minWidth: 40, maxWidth: 140, heightPercent: 80, heightMax: 60,
	}, func(width, _ int) []string {
		if cached != nil && cachedWidth == width && cachedTheme == styles.CurrentTheme() {
			return cached
		}
		cachedWidth, cachedTheme = width, styles.CurrentTheme()
		lines := []string{RenderTitle(ansi.Truncate(preview.Title, width, "…"), width, styles.DialogTitleStyle), RenderSeparator(width), ""}
		for line := range strings.SplitSeq(preview.Content, "\n") {
			lines = append(lines, strings.Split(ansi.Hardwrap(line, max(1, width), true), "\n")...)
		}
		cached = append(lines, tuiimage.RenderMarkers(picture, width)...)
		return cached
	})
	return d
}

func (d *attachmentPreviewDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	_, cmd := d.readOnlyScrollDialog.Update(msg)
	return d, cmd
}

func (d *attachmentPreviewDialog) Cleanup() {}
