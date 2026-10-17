// Package banner holds the art drawn on an empty conversation.
package banner

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

const (
	Width  = 56
	Height = 13
)

// FrameDuration is how long each frame of an animated banner is shown.
const FrameDuration = 100 * time.Millisecond

// FrameSeparator is the line that separates the frames of a banner file.
const FrameSeparator = "\f"

// maxFileSize bounds a banner file; the art is redrawn on every frame change.
const maxFileSize = 16 << 20

// Lines is the Docker Agent ASCII-art startup banner.
var Lines = []string{
	`██████╗  ██████╗  ██████╗██╗  ██╗███████╗██████╗ `,
	`██╔══██╗██╔═══██╗██╔════╝██║ ██╔╝██╔════╝██╔══██╗`,
	`██║  ██║██║   ██║██║     █████╔╝ █████╗  ██████╔╝`,
	`██║  ██║██║   ██║██║     ██╔═██╗ ██╔══╝  ██╔══██╗`,
	`██████╔╝╚██████╔╝╚██████╗██║  ██╗███████╗██║  ██║`,
	`╚═════╝  ╚═════╝  ╚═════╝╚═╝  ╚═╝╚══════╝╚═╝  ╚═╝`,
	``,
	`     █████╗  ██████╗ ███████╗███╗   ██╗████████╗`,
	`    ██╔══██╗██╔════╝ ██╔════╝████╗  ██║╚══██╔══╝`,
	`    ███████║██║  ███╗█████╗  ██╔██╗ ██║   ██║   `,
	`    ██╔══██║██║   ██║██╔══╝  ██║╚██╗██║   ██║   `,
	`    ██║  ██║╚██████╔╝███████╗██║ ╚████║   ██║   `,
	`    ╚═╝  ╚═╝ ╚═════╝ ╚══════╝╚═╝  ╚═══╝   ╚═╝   `,
}

// Banner is one or more frames of art. Every frame is padded to the same
// size, so the banner does not move as it plays.
type Banner struct {
	frames []string
	width  int
	height int
	// crops marks art that is cut down to an area smaller than itself
	// rather than left out.
	crops bool
}

// Default returns the Docker Agent banner.
func Default() Banner {
	return Banner{frames: []string{strings.Join(Lines, "\n")}, width: Width, height: Height}
}

// Load reads a banner from a text file. See Parse for the format.
func Load(path string) (Banner, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Banner{}, fmt.Errorf("reading banner file: %w", err)
	}
	if info.Size() > maxFileSize {
		return Banner{}, fmt.Errorf("banner file %s is larger than %d bytes", path, maxFileSize)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Banner{}, fmt.Errorf("reading banner file: %w", err)
	}
	b, err := Parse(string(data))
	if err != nil {
		return Banner{}, fmt.Errorf("banner file %s: %w", path, err)
	}
	return b, nil
}

// Parse builds a banner from text. A line holding only a form feed separates
// frames; a banner with several frames plays them in a loop. Lines may carry
// ANSI color sequences.
func Parse(text string) (Banner, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var frames [][]string
	var width, height int
	for chunk := range strings.SplitSeq(text, "\n"+FrameSeparator+"\n") {
		lines := strings.Split(strings.Trim(chunk, "\n"), "\n")
		if len(lines) == 1 && strings.TrimSpace(ansi.Strip(lines[0])) == "" {
			continue
		}
		height = max(height, len(lines))
		for _, line := range lines {
			width = max(width, ansi.StringWidth(line))
		}
		frames = append(frames, lines)
	}
	if len(frames) == 0 {
		return Banner{}, errors.New("no banner art found")
	}

	b := Banner{width: width, height: height, crops: true, frames: make([]string, 0, len(frames))}
	for _, lines := range frames {
		for i, line := range lines {
			lines[i] = line + strings.Repeat(" ", width-ansi.StringWidth(line))
		}
		for len(lines) < height {
			lines = append(lines, strings.Repeat(" ", width))
		}
		b.frames = append(b.frames, strings.Join(lines, "\n"))
	}
	return b, nil
}

func (b Banner) Width() int  { return b.width }
func (b Banner) Height() int { return b.height }

// Fits reports whether the banner can be drawn in the given area. Art read
// from a file always can: it is cut down around its center, so a backdrop
// larger than any terminal fills the area whatever its size.
func (b Banner) Fits(width, height int) bool {
	if width <= 0 || height <= 0 {
		return false
	}
	return b.crops || (width >= b.width && height >= b.height)
}

// Crop cuts a frame down around its center to at most the given area.
func (b Banner) Crop(frame string, width, height int) string {
	if width >= b.width && height >= b.height {
		return frame
	}
	lines := strings.Split(frame, "\n")
	if extra := len(lines) - height; extra > 0 {
		lines = lines[extra/2 : extra/2+height]
	}
	if extra := b.width - width; extra > 0 {
		for i, line := range lines {
			lines[i] = ansi.Cut(line, extra/2, extra/2+width)
		}
	}
	return strings.Join(lines, "\n")
}

// Animated reports whether the banner has more than one frame.
func (b Banner) Animated() bool { return len(b.frames) > 1 }

// FrameIndexAt returns the frame shown after the given elapsed time.
func (b Banner) FrameIndexAt(elapsed time.Duration) int {
	if len(b.frames) < 2 || elapsed <= 0 {
		return 0
	}
	return int(elapsed/FrameDuration) % len(b.frames)
}

// FrameAt returns the art shown after the given elapsed time.
func (b Banner) FrameAt(elapsed time.Duration) string {
	if len(b.frames) == 0 {
		return ""
	}
	return b.frames[b.FrameIndexAt(elapsed)]
}
