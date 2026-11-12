package image

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"
)

type overlay struct {
	id               uint32
	png              []byte
	x, y             int
	cols, rows       int
	pixelW, pixelH   int
	sourceY, sourceH int
}

// Writer adds kitty graphics after Bubble Tea has rendered its text cell buffer.
// Bubble Tea intentionally consumes APC sequences while parsing view content, so
// images must be overlaid on the completed frame instead.
type Writer struct {
	out io.Writer

	mu        sync.Mutex
	overlays  []overlay
	uploaded  map[uint32]bool
	active    bool
	dirty     bool
	enabled   bool
	supported bool
}

func NewWriter(out io.Writer) *Writer {
	return &Writer{out: out, uploaded: make(map[uint32]bool), enabled: true, supported: true}
}

// Fd, Read, and Close preserve the terminal file interface when Writer wraps
// stdout. Bubble Tea uses that interface to detect the output TTY.
func (w *Writer) Fd() uintptr {
	if file, ok := w.out.(interface{ Fd() uintptr }); ok {
		return file.Fd()
	}
	return ^uintptr(0)
}

func (w *Writer) Read(p []byte) (int, error) {
	if reader, ok := w.out.(io.Reader); ok {
		return reader.Read(p)
	}
	return 0, io.EOF
}

func (w *Writer) Close() error {
	return nil
}

// SetSupported records whether the terminal answered the Kitty graphics probe.
func (w *Writer) SetSupported(supported bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.supported != supported {
		w.supported = supported
		if !supported {
			w.overlays = nil
		}
		w.dirty = true
	}
}

// Supported reports whether startup confirmed terminal image support.
func (w *Writer) Supported() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.supported
}

// RenderingEnabled reports whether both the user setting and terminal support allow images.
func (w *Writer) RenderingEnabled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enabled && w.supported
}

// Enabled reports the user preference independently of terminal support.
func (w *Writer) Enabled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enabled
}

// SetEnabled controls whether image markers become terminal overlays.
func (w *Writer) SetEnabled(enabled bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.enabled != enabled {
		w.enabled = enabled
		if !enabled {
			w.overlays = nil
		}
		w.dirty = true
	}
}

// Invalidate forces image data and placements to be rebuilt after the terminal
// clears graphics state, such as during a resize or terminal restore.
func (w *Writer) Invalidate() {
	w.mu.Lock()
	defer w.mu.Unlock()
	clear(w.uploaded)
	w.active = false
	w.dirty = true
}

func (w *Writer) SetContent(content string) string {
	clean, overlays := extractOverlays(content)
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.enabled || !w.supported {
		overlays = nil
	}
	if !sameOverlays(w.overlays, overlays) {
		w.overlays = overlays
		w.dirty = true
	}
	return clean
}

func (w *Writer) Write(p []byte) (int, error) {
	cleared := bytes.Contains(p, []byte("\x1b[2J")) || bytes.Contains(p, []byte("\x1b[?1049h"))
	n, err := w.out.Write(p)
	if err != nil {
		return n, err
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if cleared {
		clear(w.uploaded)
		w.dirty = true
		w.active = false
	}
	if len(w.overlays) == 0 && !w.active {
		return n, nil
	}

	wasDirty := w.dirty
	newUploads := make([]uint32, 0, len(w.overlays))
	var b strings.Builder
	b.WriteString("\x1b7")
	if wasDirty {
		b.WriteString("\x1b_Ga=d,d=a,q=2\x1b\\")
	}
	for index, image := range w.overlays {
		if !w.uploaded[image.id] {
			writeTransmission(&b, image.id, image.png)
			newUploads = append(newUploads, image.id)
		}
		fmt.Fprintf(&b, "\x1b[%d;%dH", image.y+1, image.x+1)
		fmt.Fprintf(&b, "\x1b_Ga=p,i=%d,p=%d,q=2,C=1,c=%d,r=%d", image.id, index+1, image.cols, image.rows)
		if image.sourceY > 0 || image.sourceH < image.pixelH {
			fmt.Fprintf(&b, ",x=0,y=%d,w=%d,h=%d", image.sourceY, image.pixelW, image.sourceH)
		}
		b.WriteString("\x1b\\")
	}
	b.WriteString("\x1b8")
	overlay := b.String()
	written, writeErr := io.WriteString(w.out, overlay)
	if writeErr == nil && written != len(overlay) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		return n, writeErr
	}

	w.dirty = false
	w.active = len(w.overlays) > 0
	for _, id := range newUploads {
		w.uploaded[id] = true
	}
	return n, nil
}

func writeTransmission(b *strings.Builder, id uint32, png []byte) {
	encoded := base64.StdEncoding.EncodeToString(png)
	for offset := 0; offset < len(encoded); offset += kittyMaxChunkSize {
		end := min(offset+kittyMaxChunkSize, len(encoded))
		more := 0
		if end < len(encoded) {
			more = 1
		}
		if offset == 0 {
			fmt.Fprintf(b, "\x1b_Ga=t,t=d,f=100,i=%d,q=2,m=%d;%s\x1b\\", id, more, encoded[offset:end])
		} else {
			fmt.Fprintf(b, "\x1b_Gm=%d;%s\x1b\\", more, encoded[offset:end])
		}
	}
}

func sameOverlays(a, b []overlay) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].id != b[i].id || a[i].x != b[i].x || a[i].y != b[i].y ||
			a[i].cols != b[i].cols || a[i].rows != b[i].rows ||
			a[i].pixelW != b[i].pixelW || a[i].pixelH != b[i].pixelH ||
			a[i].sourceY != b[i].sourceY || a[i].sourceH != b[i].sourceH {
			return false
		}
	}
	return true
}

func extractOverlays(content string) (string, []overlay) {
	lines := strings.Split(content, "\n")
	overlays := extractMarkerOverlays(lines)
	for y, line := range lines {
		for {
			start := strings.Index(line, "\x1b_G")
			if start < 0 {
				break
			}
			x := ansi.StringWidth(line[:start])
			var encoded strings.Builder
			cols, rows := 0, 0
			end := start
			for strings.HasPrefix(line[end:], "\x1b_G") {
				stop := strings.Index(line[end+3:], "\x1b\\")
				if stop < 0 {
					end += 3
					break
				}
				stop += end + 3
				body := line[end+3 : stop]
				params, payload, _ := strings.Cut(body, ";")
				for param := range strings.SplitSeq(params, ",") {
					key, value, ok := strings.Cut(param, "=")
					if !ok {
						continue
					}
					switch key {
					case "c":
						cols, _ = strconv.Atoi(value)
					case "r":
						rows, _ = strconv.Atoi(value)
					}
				}
				encoded.WriteString(payload)
				end = stop + 2
			}
			data, err := base64.StdEncoding.DecodeString(encoded.String())
			if err == nil && len(data) > 0 && cols > 0 && rows > 0 {
				id := imageID(data)
				overlays = append(overlays, overlay{id: id, png: data, x: x, y: y, cols: cols, rows: rows})
			}
			line = line[:start] + line[end:]
		}
		lines[y] = line
	}
	return strings.Join(lines, "\n"), overlays
}

func extractMarkerOverlays(lines []string) []overlay {
	var overlays []overlay
	for y, line := range lines {
		for {
			start := strings.Index(line, markerPrefix)
			if start < 0 {
				break
			}
			stopRel := strings.Index(line[start+len(markerPrefix):], "\x1b\\")
			if stopRel < 0 {
				line = line[:start] + line[start+len(markerPrefix):]
				continue
			}
			stop := start + len(markerPrefix) + stopRel
			fields := strings.Split(line[start+len(markerPrefix):stop], ";")
			if len(fields) == 4 {
				id64, idErr := strconv.ParseUint(fields[0], 10, 32)
				cols, colsErr := strconv.Atoi(fields[1])
				totalRows, rowsErr := strconv.Atoi(fields[2])
				row, rowErr := strconv.Atoi(fields[3])
				img, ok := registeredImage(uint32(id64))
				if idErr == nil && colsErr == nil && rowsErr == nil && rowErr == nil && ok && totalRows > 0 {
					x := ansi.StringWidth(line[:start])
					if len(overlays) > 0 {
						last := &overlays[len(overlays)-1]
						lastEndRow := last.sourceY + last.sourceH
						expectedSourceY := img.Height * row / totalRows
						if uint64(last.id) == id64 && last.x == x && last.y+last.rows == y && lastEndRow == expectedSourceY {
							last.rows++
							last.sourceH = img.Height*(row+1)/totalRows - last.sourceY
						} else {
							overlays = append(overlays, markerOverlay(uint32(id64), img, x, y, cols, totalRows, row))
						}
					} else {
						overlays = append(overlays, markerOverlay(uint32(id64), img, x, y, cols, totalRows, row))
					}
				}
			}
			line = line[:start] + line[stop+2:]
		}
		lines[y] = line
	}
	return overlays
}

func markerOverlay(id uint32, img Inline, x, y, cols, totalRows, row int) overlay {
	sourceY := img.Height * row / totalRows
	sourceEnd := img.Height * (row + 1) / totalRows
	return overlay{
		id: id, png: img.PNGData, x: x, y: y, cols: cols, rows: 1,
		pixelW: img.Width, pixelH: img.Height,
		sourceY: sourceY, sourceH: sourceEnd - sourceY,
	}
}
