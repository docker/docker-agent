package chat

import (
	"fmt"
	"net/http"
)

const (
	// MaxImageDimension is the maximum width or height for resized images.
	MaxImageDimension = 2000
	// MaxImageBytes is the maximum size of a resized image in bytes.
	MaxImageBytes = 4_500_000
)

// DetectMimeTypeByContent detects the MIME type of data by inspecting its content
// (magic bytes). Returns the MIME type or "application/octet-stream" if unknown.
// This supplements extension-based detection for files with missing or wrong extensions.
func DetectMimeTypeByContent(data []byte) string {
	if len(data) == 0 {
		return "application/octet-stream"
	}

	// Check WebP first — http.DetectContentType doesn't recognise it.
	if len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp"
	}

	ct := http.DetectContentType(data)

	// http.DetectContentType returns types like "image/jpeg", "image/png",
	// "image/gif", "application/pdf", etc. For our purposes we accept it as-is.
	return ct
}

// ImageResizeResult holds the output of an image resize operation.
type ImageResizeResult struct {
	// Data is the (possibly re-encoded) image bytes.
	Data []byte
	// MimeType is the MIME type of the output image.
	MimeType string
	// OriginalWidth and OriginalHeight are the dimensions of the input image.
	OriginalWidth, OriginalHeight int
	// Width and Height are the dimensions of the output image.
	Width, Height int
	// Resized indicates whether the image was actually modified.
	Resized bool
}

// FormatDimensionNote produces a human-readable note describing the resize mapping.
// This helps the model translate coordinates from the resized image back to the original.
//
// Because ResizeImage uses fitDimensions (which preserves aspect ratio), the X and
// Y scale factors are always equal in practice. If they ever differ (e.g. because
// the function is called with a manually constructed ImageResizeResult), we emit
// separate per-axis factors so that coordinate mapping remains correct.
func FormatDimensionNote(r *ImageResizeResult) string {
	if !r.Resized {
		return ""
	}
	scaleX := float64(r.OriginalWidth) / float64(r.Width)
	scaleY := float64(r.OriginalHeight) / float64(r.Height)

	// Uniform scaling (the normal path): a single factor suffices.
	const epsilon = 0.01
	if abs(scaleX-scaleY) < epsilon {
		return fmt.Sprintf("[Image: original %dx%d, displayed at %dx%d. Multiply coordinates by %.2f to map to original image.]",
			r.OriginalWidth, r.OriginalHeight, r.Width, r.Height, scaleX)
	}

	// Non-uniform scaling: provide separate X and Y factors.
	return fmt.Sprintf("[Image: original %dx%d, displayed at %dx%d. Multiply X coordinates by %.2f and Y coordinates by %.2f to map to original image.]",
		r.OriginalWidth, r.OriginalHeight, r.Width, r.Height, scaleX, scaleY)
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
