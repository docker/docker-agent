// Package image prepares and renders images for terminal display.
package image

import (
	"bytes"
	"container/list"
	"context"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	stdimage "image"
	_ "image/gif"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yuin/goldmark"
	goldmarkast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

var markdownImageRangePattern = regexp.MustCompile(`!\[([^\]\n]*)\]\([ \t]*(?:<([^>\n]+)>|([^ \t)\n]+))(?:[ \t]+(?:"[^"\n]*"|'[^'\n]*'|\([^\)\n]*\)))?[ \t]*\)`)

const (
	kittyMaxChunkSize        = 4096
	maxImageCols             = 60
	maxImageRows             = 20
	maxInlineRegistryEntries = 128
	markerPrefix             = "\x1b_cagent-image;"
	maxMarkdownImageBytes    = 20 << 20
)

type registryEntry struct {
	id    uint32
	image Inline
}

var (
	inlineRegistry = struct {
		sync.Mutex

		entries map[uint32]*list.Element
		order   list.List
	}{entries: make(map[uint32]*list.Element)}
	renderingDisabled atomic.Bool
)

// SetRenderingEnabled controls whether the full-screen TUI reserves image rows.
func SetRenderingEnabled(enabled bool) {
	renderingDisabled.Store(!enabled)
}

// Inline is a PNG image prepared for inline kitty-protocol rendering.
type Inline struct {
	Name    string
	MIME    string
	PNGData []byte
	Width   int
	Height  int
}

// FromToolResult extracts displayable images from a tool result.
func FromToolResult(result *tools.ToolCallResult) []Inline {
	if result == nil {
		return nil
	}

	images := make([]Inline, 0, len(result.Images)+len(result.Documents))
	for i, img := range result.Images {
		name := fmt.Sprintf("image-%d", i+1)
		if inline, ok := FromBase64(name, img.MimeType, img.Data); ok {
			images = append(images, inline)
		}
	}
	for _, doc := range result.Documents {
		if !chat.IsImageMimeType(doc.MimeType) || doc.Data == "" {
			continue
		}
		name := doc.Name
		if name == "" {
			name = "image"
		}
		if inline, ok := FromBase64(name, doc.MimeType, doc.Data); ok {
			images = append(images, inline)
		}
	}
	return images
}

// MarkdownReference is an image embedded with Markdown's ![alt](source) syntax.
type MarkdownReference struct {
	Alt    string
	Source string
	Start  int
	End    int
}

// MarkdownReferences extracts image references in document order.
func MarkdownReferences(markdown string) []MarkdownReference {
	// Plain text is overwhelmingly common during progressive streaming; avoid
	// constructing a Goldmark AST until an image opener is even possible.
	if !strings.Contains(markdown, "![") {
		return nil
	}
	source := []byte(markdown)
	document := goldmark.DefaultParser().Parse(text.NewReader(source))
	var refs []MarkdownReference
	_ = goldmarkast.Walk(document, func(node goldmarkast.Node, entering bool) (goldmarkast.WalkStatus, error) {
		if entering {
			if image, ok := node.(*goldmarkast.Image); ok {
				ref := MarkdownReference{
					Alt:    markdownImageAlt(image, source),
					Source: string(image.Destination),
				}
				ref.Start, ref.End = markdownImageRange(markdown, image.Pos(), ref.Source)
				refs = append(refs, ref)
			}
		}
		return goldmarkast.WalkContinue, nil
	})
	return refs
}

func markdownImageRange(markdown string, position int, source string) (int, int) {
	for _, match := range markdownImageRangePattern.FindAllStringSubmatchIndex(markdown, -1) {
		if position < match[0] || position > match[1] {
			continue
		}
		sourceStart, sourceEnd := match[4], match[5]
		if sourceStart < 0 {
			sourceStart, sourceEnd = match[6], match[7]
		}
		if markdown[sourceStart:sourceEnd] == source {
			return markdownLinkedImageRange(markdown, match[0], match[1])
		}
	}
	return -1, -1
}

func markdownLinkedImageRange(markdown string, start, end int) (int, int) {
	if start == 0 || markdown[start-1] != '[' || end+2 > len(markdown) || markdown[end:end+2] != "](" {
		return start, end
	}
	depth := 1
	for i := end + 2; i < len(markdown); i++ {
		switch markdown[i] {
		case '\\':
			i++
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return start - 1, i + 1
			}
		}
	}
	return start, end
}

func markdownImageAlt(image *goldmarkast.Image, source []byte) string {
	var alt strings.Builder
	_ = goldmarkast.Walk(image, func(node goldmarkast.Node, entering bool) (goldmarkast.WalkStatus, error) {
		if !entering || node == image {
			return goldmarkast.WalkContinue, nil
		}
		switch node := node.(type) {
		case *goldmarkast.Text:
			alt.Write(node.Value(source))
		case *goldmarkast.String:
			alt.Write(node.Value)
		}
		return goldmarkast.WalkContinue, nil
	})
	return alt.String()
}

// LoadMarkdownReference resolves a data URI, local path, or public HTTP URL.
func LoadMarkdownReference(ctx context.Context, ref MarkdownReference) (Inline, bool) {
	name := ref.Alt
	if name == "" {
		name = filepath.Base(ref.Source)
	}

	var data []byte
	var mimeType string
	if strings.HasPrefix(ref.Source, "data:") {
		comma := strings.IndexByte(ref.Source, ',')
		if comma < 0 || !strings.HasSuffix(ref.Source[:comma], ";base64") {
			return Inline{}, false
		}
		mimeType = strings.TrimSuffix(strings.TrimPrefix(ref.Source[:comma], "data:"), ";base64")
		decoded, err := base64.StdEncoding.DecodeString(ref.Source[comma+1:])
		if err != nil || len(decoded) > maxMarkdownImageBytes {
			return Inline{}, false
		}
		data = decoded
	} else if parsed, err := url.Parse(ref.Source); err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		client := httpclient.NewSafeClient(10*time.Second, false)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.Source, http.NoBody)
		if err != nil {
			return Inline{}, false
		}
		resp, err := client.Do(req)
		if err != nil {
			return Inline{}, false
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return Inline{}, false
		}
		mimeType = strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
		limited := io.LimitReader(resp.Body, maxMarkdownImageBytes+1)
		data, err = io.ReadAll(limited)
		if err != nil || len(data) > maxMarkdownImageBytes {
			return Inline{}, false
		}
	} else {
		// Sources come from LLM-generated markdown, so local schemes like
		// file:// and sandbox:// would let a prompt-injected response read
		// arbitrary local files (confused deputy). Only bare paths pass.
		// Absolute Windows paths (C:\…) are bare paths too, even though
		// net/url reads their drive letter as a single-letter scheme.
		if parsed != nil && parsed.Scheme != "" && !isWindowsDrivePath(ref.Source) {
			return Inline{}, false
		}
		file, err := os.Open(ref.Source)
		if err != nil {
			return Inline{}, false
		}
		defer file.Close()
		data, err = io.ReadAll(io.LimitReader(file, maxMarkdownImageBytes+1))
		if err != nil || len(data) > maxMarkdownImageBytes {
			return Inline{}, false
		}
	}

	return FromBytes(name, mimeType, data)
}

// isWindowsDrivePath reports whether source is an absolute Windows path
// (`C:\...` or `C:/...`), whose drive letter net/url would misparse as a
// URL scheme. Real URI schemes are never a single letter, so this cannot
// re-admit file:// or sandbox:// sources.
func isWindowsDrivePath(source string) bool {
	if len(source) < 3 || source[1] != ':' || (source[2] != '\\' && source[2] != '/') {
		return false
	}
	drive := source[0]
	return ('a' <= drive && drive <= 'z') || ('A' <= drive && drive <= 'Z')
}

// FromBase64 decodes and normalizes one base64-encoded image.
func FromBase64(name, mimeType, encoded string) (Inline, bool) {
	if strings.TrimSpace(encoded) == "" {
		return Inline{}, false
	}

	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return Inline{}, false
	}
	return FromBytes(name, mimeType, data)
}

// FromBytes normalizes encoded image bytes for terminal rendering.
func FromBytes(name, mimeType string, data []byte) (Inline, bool) {
	if !chat.IsImageMimeType(mimeType) {
		mimeType = chat.DetectMimeTypeByContent(data)
	}
	if !chat.IsImageMimeType(mimeType) {
		return Inline{}, false
	}
	if resized, resizeErr := chat.ResizeImage(data, mimeType); resizeErr == nil {
		data = resized.Data
		mimeType = resized.MimeType
	}

	img, _, err := stdimage.Decode(bytes.NewReader(data))
	if err != nil {
		return Inline{}, false
	}
	bounds := img.Bounds()

	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		return Inline{}, false
	}

	return Inline{
		Name:    name,
		MIME:    mimeType,
		PNGData: pngBuf.Bytes(),
		Width:   bounds.Dx(),
		Height:  bounds.Dy(),
	}, true
}

// Render returns a label, kitty image sequence, and the rows reserved by it.
func Render(img Inline, width int) []string {
	if len(img.PNGData) == 0 || img.Width <= 0 || img.Height <= 0 || width <= 0 {
		return nil
	}

	cols, rows := cellSize(img, width)

	label := "image"
	if img.Name != "" {
		label = img.Name
	}
	if img.MIME != "" {
		label += " (" + img.MIME + ")"
	}

	out := []string{"  " + styles.MutedStyle.Render("🖼 "+label)}
	out = append(out, "  "+KittySequence(img.PNGData, cols, rows))
	for range rows - 1 {
		out = append(out, "")
	}
	return out
}

// RenderMarkers renders a compact marker on every occupied row. The normal
// TUI resolves these after viewport clipping, which lets it crop images that
// are partially scrolled off-screen without copying PNG data into every row.
func RenderMarkers(img Inline, width int) []string {
	if renderingDisabled.Load() || len(img.PNGData) == 0 || img.Width <= 0 || img.Height <= 0 || width <= 0 {
		return nil
	}
	cols, rows := cellSize(img, width)
	id := registerImage(imageID(img.PNGData), img)

	out := make([]string, 0, rows)
	for row := range rows {
		out = append(out, fmt.Sprintf("  %s%d;%d;%d;%d\x1b\\", markerPrefix, id, cols, rows, row))
	}
	return out
}

func cellSize(img Inline, width int) (cols, rows int) {
	cols = min(max(width-4, 1), maxImageCols)
	rows = max(1, (img.Height*cols+2*img.Width-1)/(2*img.Width))
	if rows <= maxImageRows {
		return cols, rows
	}

	// Shrink both dimensions together. Capping rows alone stretches tall and
	// square images horizontally because terminal cells are roughly twice as
	// tall as they are wide.
	cols = min(cols, max(1, maxImageRows*2*img.Width/img.Height))
	rows = min(maxImageRows, max(1, (img.Height*cols+2*img.Width-1)/(2*img.Width)))
	return cols, rows
}

func imageID(data []byte) uint32 {
	h := fnv.New32a()
	_, _ = h.Write(data)
	if id := h.Sum32(); id != 0 {
		return id
	}
	return 1
}

func registerImage(id uint32, img Inline) uint32 {
	inlineRegistry.Lock()
	defer inlineRegistry.Unlock()
	for {
		if elem, ok := inlineRegistry.entries[id]; ok {
			entry := elem.Value.(*registryEntry)
			if bytes.Equal(entry.image.PNGData, img.PNGData) {
				entry.image = img
				inlineRegistry.order.MoveToFront(elem)
				return id
			}
			id++
			if id == 0 {
				id = 1
			}
			continue
		}

		elem := inlineRegistry.order.PushFront(&registryEntry{id: id, image: img})
		inlineRegistry.entries[id] = elem
		if inlineRegistry.order.Len() > maxInlineRegistryEntries {
			oldest := inlineRegistry.order.Back()
			inlineRegistry.order.Remove(oldest)
			delete(inlineRegistry.entries, oldest.Value.(*registryEntry).id)
		}
		return id
	}
}

func registeredImage(id uint32) (Inline, bool) {
	inlineRegistry.Lock()
	defer inlineRegistry.Unlock()
	elem, ok := inlineRegistry.entries[id]
	if !ok {
		return Inline{}, false
	}
	inlineRegistry.order.MoveToFront(elem)
	return elem.Value.(*registryEntry).image, true
}

// KittySequence encodes PNG data as a kitty graphics escape sequence.
func KittySequence(pngData []byte, cols, rows int) string {
	encoded := base64.StdEncoding.EncodeToString(pngData)
	var b strings.Builder
	for offset := 0; offset < len(encoded); offset += kittyMaxChunkSize {
		end := min(offset+kittyMaxChunkSize, len(encoded))
		more := 0
		if end < len(encoded) {
			more = 1
		}
		if offset == 0 {
			fmt.Fprintf(&b, "\x1b_Ga=T,t=d,f=100,q=2,C=1,c=%d,r=%d,m=%d;%s\x1b\\", cols, rows, more, encoded[offset:end])
		} else {
			fmt.Fprintf(&b, "\x1b_Gm=%d;%s\x1b\\", more, encoded[offset:end])
		}
	}
	return b.String()
}
