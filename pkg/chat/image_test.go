package chat

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectMimeTypeByContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"empty", nil, "application/octet-stream"},
		{"png", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, "image/png"},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0}, "image/jpeg"},
		{"gif87a", []byte("GIF87a"), "image/gif"},
		{"gif89a", []byte("GIF89a"), "image/gif"},
		{"webp", append([]byte("RIFF"), append([]byte{0, 0, 0, 0}, []byte("WEBP")...)...), "image/webp"},
		{"pdf", []byte("%PDF-1.4"), "application/pdf"},
		{"text", []byte("Hello, world!"), "text/plain; charset=utf-8"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := DetectMimeTypeByContent(tt.data)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFitDimensions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		w, h         int
		maxW, maxH   int
		wantW, wantH int
	}{
		{"already fits", 800, 600, 2000, 2000, 800, 600},
		{"too wide", 4000, 2000, 2000, 2000, 2000, 1000},
		{"too tall", 1000, 4000, 2000, 2000, 500, 2000},
		{"both too large", 4000, 3000, 2000, 2000, 2000, 1500},
		{"square", 3000, 3000, 2000, 2000, 2000, 2000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotW, gotH := fitDimensions(tt.w, tt.h, tt.maxW, tt.maxH)
			assert.Equal(t, tt.wantW, gotW, "width")
			assert.Equal(t, tt.wantH, gotH, "height")
		})
	}
}

// createTestPNG creates a solid-color PNG image of the given dimensions.
func createTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func createTestJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: 0, G: 128, B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}))
	return buf.Bytes()
}

func TestResizeImage_NoResizeNeeded(t *testing.T) {
	t.Parallel()

	data := createTestPNG(t, 100, 100)
	result, err := ResizeImage(data, "image/png")
	require.NoError(t, err)

	assert.False(t, result.Resized)
	assert.Equal(t, data, result.Data)
	assert.Equal(t, "image/png", result.MimeType)
	assert.Equal(t, 100, result.Width)
	assert.Equal(t, 100, result.Height)
	assert.Equal(t, 100, result.OriginalWidth)
	assert.Equal(t, 100, result.OriginalHeight)
}

func TestResizeImage_ScalesDown(t *testing.T) {
	t.Parallel()

	// Create an image larger than MaxImageDimension.
	data := createTestPNG(t, 4000, 3000)
	result, err := ResizeImage(data, "image/png")
	require.NoError(t, err)

	assert.True(t, result.Resized)
	assert.Equal(t, 4000, result.OriginalWidth)
	assert.Equal(t, 3000, result.OriginalHeight)
	assert.Equal(t, 2000, result.Width)
	assert.Equal(t, 1500, result.Height)
	assert.NotEmpty(t, result.Data)
}

func TestResizeImage_JPEG(t *testing.T) {
	t.Parallel()

	data := createTestJPEG(t, 3000, 2000)
	result, err := ResizeImage(data, "image/jpeg")
	require.NoError(t, err)

	assert.True(t, result.Resized)
	assert.Equal(t, 2000, result.Width)
	assert.LessOrEqual(t, result.Height, 1334) // 2000 * (2000/3000) ≈ 1333
}

func TestFormatDimensionNote(t *testing.T) {
	t.Parallel()

	t.Run("not resized", func(t *testing.T) {
		t.Parallel()
		result := &ImageResizeResult{Resized: false}
		assert.Empty(t, FormatDimensionNote(result))
	})

	t.Run("uniform scaling", func(t *testing.T) {
		t.Parallel()
		result := &ImageResizeResult{
			Resized:        true,
			OriginalWidth:  4000,
			OriginalHeight: 3000,
			Width:          2000,
			Height:         1500,
		}
		note := FormatDimensionNote(result)
		assert.Contains(t, note, "original 4000x3000")
		assert.Contains(t, note, "displayed at 2000x1500")
		assert.Contains(t, note, "Multiply coordinates by 2.00")
		// Should NOT contain separate X/Y factors.
		assert.NotContains(t, note, "X coordinates")
	})

	t.Run("non-uniform scaling", func(t *testing.T) {
		t.Parallel()
		// Manually constructed result with different X and Y scale factors.
		result := &ImageResizeResult{
			Resized:        true,
			OriginalWidth:  4000,
			OriginalHeight: 2000,
			Width:          2000,
			Height:         2000,
		}
		note := FormatDimensionNote(result)
		assert.Contains(t, note, "original 4000x2000")
		assert.Contains(t, note, "displayed at 2000x2000")
		assert.Contains(t, note, "Multiply X coordinates by 2.00")
		assert.Contains(t, note, "Y coordinates by 1.00")
	})
}

func TestResizeImageBase64(t *testing.T) {
	t.Parallel()

	data := createTestPNG(t, 100, 100)
	b64 := base64.StdEncoding.EncodeToString(data)

	b64Result, result, err := ResizeImageBase64(b64, "image/png")
	require.NoError(t, err)
	assert.False(t, result.Resized)
	// Result data stays as raw bytes
	assert.Equal(t, data, result.Data)
	// Base64 result is returned separately
	assert.Equal(t, b64, b64Result)
}
