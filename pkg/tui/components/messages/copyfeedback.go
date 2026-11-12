package messages

import (
	"image/color"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// copiedFlashDuration is how long a clicked copy label reads "copied" before
// reverting.
const copiedFlashDuration = 1500 * time.Millisecond

// copiedFlash tracks the transient "copied" confirmation shown in place of a
// copy label after it is clicked. The label is addressed by message index and
// line within that message's view so it stays put when content above shifts.
type copiedFlash struct {
	msgIdx    int
	localLine int
	codeBlock bool
	seq       int
}

// copiedFlashExpiredMsg reverts the "copied" confirmation once the flash
// duration elapsed. Seq guards against reverting a newer flash.
type copiedFlashExpiredMsg struct {
	Seq int
}

// flashCopiedLabel swaps the clicked copy label for a transient "copied"
// confirmation and schedules its revert.
func (m *model) flashCopiedLabel(msgIdx, localLine int, codeBlock bool) tea.Cmd {
	m.copiedFlashSeq++
	seq := m.copiedFlashSeq
	m.copiedFlash = &copiedFlash{msgIdx: msgIdx, localLine: localLine, codeBlock: codeBlock, seq: seq}
	return tea.Tick(copiedFlashDuration, func(time.Time) tea.Msg {
		return copiedFlashExpiredMsg{Seq: seq}
	})
}

// handleCopiedFlashExpired clears the flash unless a newer one replaced it.
func (m *model) handleCopiedFlashExpired(msg copiedFlashExpiredMsg) {
	if m.copiedFlash != nil && m.copiedFlash.seq == msg.Seq {
		m.copiedFlash = nil
	}
}

// applyCopiedFlash replaces the flashed copy label with the same-width
// "copied" confirmation on the visible line it lives on. It is a view-time
// overlay: rendered caches are left untouched and the swap disappears on its
// own once the flash state is cleared.
func (m *model) applyCopiedFlash(lines []string, viewportStartLine int) []string {
	f := m.copiedFlash
	if f == nil || f.msgIdx < 0 || f.msgIdx >= len(m.lineOffsets) {
		return lines
	}
	idx := m.lineOffsets[f.msgIdx] + f.localLine - viewportStartLine
	if idx < 0 || idx >= len(lines) {
		return lines
	}

	label := types.MessageCopyLabel
	if f.codeBlock {
		label = markdown.CodeBlockCopyIcon
	}

	line := lines[idx]
	plain := ansi.Strip(line)
	before, _, ok := strings.Cut(plain, label)
	if !ok {
		return lines
	}
	start := ansi.StringWidth(before)
	end := start + ansi.StringWidth(label)

	style := styles.SuccessStyle.Bold(true)
	// Keep whatever band the copy label sat on (user message background,
	// code block background) intact behind the swap.
	if bg := backgroundAt(line, start); bg != nil {
		style = style.Background(bg)
	}

	result := slices.Clip(slices.Clone(lines))
	result[idx] = ansi.Cut(line, 0, start) +
		style.Render(types.CopiedFeedbackLabel) +
		ansi.Cut(line, end, ansi.StringWidth(plain))
	return result
}

// backgroundAt returns the SGR background color in effect at the given cell
// column of a rendered line, or nil when none is set.
func backgroundAt(line string, col int) color.Color {
	p := ansi.GetParser()
	defer ansi.PutParser(p)

	var bg color.Color
	var state byte
	for w := 0; line != "" && w <= col; {
		seq, width, n, newState := ansi.DecodeSequence(line, state, p)
		if n == 0 {
			break // invalid input that cannot be decoded; avoid spinning forever
		}
		if ansi.HasCsiPrefix(seq) && p.Command() == 'm' {
			bg = applySGRBackground(bg, p.Params())
		}
		w += width
		state = newState
		line = line[n:]
	}
	return bg
}

// applySGRBackground folds one SGR sequence's background-related parameters
// into the current background, ignoring everything else.
func applySGRBackground(bg color.Color, params ansi.Params) color.Color {
	if len(params) == 0 {
		return nil // bare ESC[m is a full reset
	}
	for i := 0; i < len(params); i++ {
		switch param := params[i].Param(0); {
		case param == 0 || param == 49:
			bg = nil
		case param >= 40 && param <= 47:
			bg = ansi.Black + ansi.BasicColor(param-40)
		case param >= 100 && param <= 107:
			bg = ansi.BrightBlack + ansi.BasicColor(param-100)
		case param == 48:
			var c color.Color
			n := ansi.ReadStyleColor(params[i:], &c)
			if n == 0 {
				return bg // malformed extended color; stop parsing this sequence
			}
			bg = c
			i += n - 1
		}
	}
	return bg
}
