package editor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"
	"github.com/docker/go-units"
	"github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"

	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/editor/completions"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/internal/termfeatures"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// ansiRegexp matches ANSI escape sequences so they can be removed when
// computing layout measurements.
var ansiRegexp = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

const (
	// maxInlinePasteLines is the maximum number of lines for inline paste.
	// Pastes exceeding this are buffered to a temp file attachment.
	maxInlinePasteLines = 5
	// maxInlinePasteChars is the character limit for inline pastes.
	// This catches very long single-line pastes that would clutter the editor.
	maxInlinePasteChars = 500
)

type attachment struct {
	path        string // Path to file (temp for pastes, real for file refs)
	placeholder string // @paste-1 or @filename
	label       string // Display label like "paste-1 (21.1 KB)"
	sizeBytes   int
	isTemp      bool // True for paste temp files that need cleanup
}

// AttachmentPreview describes an attachment and its contents for dialog display.
type AttachmentPreview struct {
	Title   string
	Content string
}

// Editor represents an input editor component
type Editor interface {
	layout.Model
	layout.Sizeable
	layout.Focusable
	SetWorking(working bool) tea.Cmd
	AcceptSuggestion() tea.Cmd
	// TryStartArgumentCompletion opens the argument-completion popup for the
	// slash command at the start of the editor's value (e.g.
	// "/toolset-restart "), if that command exposes argument candidates.
	// Called from the Tab/switchFocus path, never from editor keypress
	// handling, so a bare space after a command never auto-opens a popup —
	// only Tab does. Returns nil (leaving editor state untouched) when
	// there's nothing to complete, so the caller can fall through to its own
	// Tab handling (e.g. focus switch).
	TryStartArgumentCompletion() tea.Cmd
	ScrollByWheel(delta int)
	// Value returns the current editor content
	Value() string
	// SetValue updates the editor content
	SetValue(content string)
	// InsertText inserts text at the current cursor position
	InsertText(text string)
	// AttachFile adds a file as an attachment and inserts @filepath into the editor
	AttachFile(filePath string) error
	Cleanup()
	GetSize() (width, height int)
	BannerHeight() int
	AttachmentAt(x int) (AttachmentPreview, bool)
	// SetRecording sets the recording mode which shows animated dots as the cursor
	SetRecording(recording bool) tea.Cmd
	// IsRecording returns true if the editor is in recording mode
	IsRecording() bool
	// IsHistorySearchActive returns true if the editor is in history search mode
	IsHistorySearchActive() bool
	// EnterHistorySearch activates incremental history search
	EnterHistorySearch() (layout.Model, tea.Cmd)
	// SendContent triggers sending the current editor content
	SendContent() tea.Cmd
}

// fileLoadResultMsg is sent when async file loading completes.
type fileLoadResultMsg struct {
	loadID     uint64
	items      []completion.Item
	isFullLoad bool // true for full load, false for initial shallow load
}

// historySearchState holds the state for incremental history search.
type historySearchState struct {
	active                   bool
	query                    string
	origTextValue            string
	origTextPlaceholderValue string
	match                    string
	matchIndex               int
	failing                  bool
}

// editor implements [Editor]
type editor struct {
	textarea textarea.Model
	hist     *history.History
	width    int
	height   int
	working  bool
	// completions are the available completions
	completions []completions.Completion

	// completionWord stores the word being completed
	completionWord    string
	currentCompletion completions.Completion
	// argumentPrefix is the exact "/slashCommand " prefix active during an
	// argument-completion session (non-empty only then). It disambiguates
	// full-line-splice argument selection (see the completion.SelectedMsg
	// case) from the trigger-word splice used for name completion (/, @).
	argumentPrefix string

	suggestion    string
	hasSuggestion bool
	// userTyped tracks whether the user has manually typed content (vs loaded from history)
	userTyped bool
	// keyboardEnhancementsSupported tracks whether the terminal supports keyboard enhancements
	keyboardEnhancementsSupported bool
	// pendingFileRef tracks the current @word being typed (for manual file ref detection).
	// Only set when cursor is in a word starting with @, cleared when cursor leaves.
	pendingFileRef string
	// banner renders pending attachments so the user can see what's queued.
	banner *attachmentBanner
	// attachments tracks all file attachments (pastes and file refs).
	attachments []attachment
	// pasteCounter tracks the next paste number for display purposes.
	pasteCounter int
	// recording tracks whether the editor is in recording mode (speech-to-text)
	recording bool
	// placeholder is the configured empty-editor placeholder, restored when
	// transient placeholders (e.g. recording mode) end.
	placeholder string
	// recordingDotPhase tracks the animation phase for the recording dots cursor
	recordingDotPhase int

	// fileLoadID is incremented each time we start a new file load to ignore stale results
	fileLoadID uint64
	// fileLoadStarted tracks whether we've started initial loading for the current completion
	fileLoadStarted bool
	// fileFullLoadStarted tracks whether we've started full file loading (triggered by typing)
	fileFullLoadStarted bool
	// fileLoadCancel cancels any in-progress file loading
	fileLoadCancel context.CancelFunc

	// historySearch holds state for history search mode
	historySearch historySearchState
	// searchInput is the input field for history search queries
	searchInput textinput.Model
}

// Option configures the Editor.
type Option func(*editor)

// WithCompletions sets the available completions for the editor.
func WithCompletions(comps ...completions.Completion) Option {
	return func(e *editor) {
		e.completions = comps
	}
}

// WithReadOnly disables the editor so no new messages can be composed.
func WithReadOnly() Option {
	return func(e *editor) {
		e.textarea.Placeholder = "Session is read-only"
		e.textarea.KeyMap.InsertNewline.SetEnabled(false)
	}
}

// defaultPlaceholder is shown in an empty editor unless WithPlaceholder
// overrides it.
const defaultPlaceholder = "Type your message here…"

// WithPlaceholder sets the editor's placeholder text (shown while empty).
func WithPlaceholder(placeholder string) Option {
	return func(e *editor) {
		e.placeholder = placeholder
		e.textarea.Placeholder = placeholder
	}
}

// searchInputStyles returns the history-search input's styling, muted
// relative to the main textarea to distinguish search mode. Shared by New
// and the ThemeChangedMsg handler so both stay in sync with the active theme.
func searchInputStyles() textinput.Styles {
	s := styles.DialogInputStyle
	s.Focused.Text = styles.MutedStyle
	s.Focused.Placeholder = styles.MutedStyle
	s.Blurred.Text = styles.MutedStyle
	s.Blurred.Placeholder = styles.MutedStyle
	return s
}

// New creates a new editor component
func New(hist *history.History, opts ...Option) Editor {
	ta := textarea.New()
	ta.SetStyles(styles.InputStyle)
	ta.Placeholder = defaultPlaceholder
	ta.Prompt = ""
	ta.CharLimit = -1
	ta.SetWidth(50)
	ta.SetHeight(3) // Set minimum 3 lines for multi-line input
	ta.Focus()
	ta.ShowLineNumbers = false

	si := textinput.New()
	si.Prompt = ""
	si.Placeholder = "Type to search..."
	si.SetStyles(searchInputStyles())

	e := &editor{
		textarea:                      ta,
		searchInput:                   si,
		hist:                          hist,
		placeholder:                   defaultPlaceholder,
		keyboardEnhancementsSupported: termfeatures.SupportsModifiedEnter(os.Getenv),
		banner:                        newAttachmentBanner(),
	}

	// Apply options
	for _, opt := range opts {
		opt(e)
	}

	e.configureNewlineKeybinding()

	return e
}

// Init initializes the component
func (e *editor) Init() tea.Cmd {
	return textarea.Blink
}

// stripANSI removes ANSI escape sequences from the provided string so width
// calculations can be performed on plain text.
func stripANSI(s string) string {
	return ansiRegexp.ReplaceAllString(s, "")
}

// lineHasContent reports whether the rendered line has user input after the
// prompt has been stripped.
func lineHasContent(line, prompt string) bool {
	plain := stripANSI(line)
	if prompt != "" && strings.HasPrefix(plain, prompt) {
		plain = strings.TrimPrefix(plain, prompt)
	}

	return strings.TrimSpace(plain) != ""
}

// extractLineText extracts the user input text from a rendered view line,
// stripping ANSI codes and the prompt prefix.
func extractLineText(line, prompt string) string {
	plain := stripANSI(line)
	if prompt != "" && strings.HasPrefix(plain, prompt) {
		plain = strings.TrimPrefix(plain, prompt)
	}
	return strings.TrimRight(plain, " ")
}

// computeWrappedLines uses a textarea to compute how text would be wrapped,
// matching the textarea's word-wrap behavior exactly.
func (e *editor) computeWrappedLines(text string, startOffset int) []string {
	// Create a temporary textarea with the same settings
	ta := textarea.New()
	ta.Prompt = e.textarea.Prompt
	ta.ShowLineNumbers = e.textarea.ShowLineNumbers
	ta.SetWidth(e.textarea.Width())
	ta.SetHeight(100) // Large enough to see all wrapped lines

	// For the first line, we need to account for the cursor position.
	// We do this by prefixing with spaces to simulate the existing text.
	prefix := strings.Repeat(" ", startOffset)
	ta.SetValue(prefix + text)

	view := ta.View()
	viewLines := strings.Split(view, "\n")

	// Extract the text content from each visual line
	var result []string
	for i, line := range viewLines {
		plain := extractLineText(line, ta.Prompt)
		if i == 0 {
			// First line: remove the prefix spaces we added
			if len(plain) >= startOffset {
				plain = plain[startOffset:]
			}
		}
		// Stop at empty lines (end of content)
		if plain == "" && i > 0 {
			break
		}
		result = append(result, plain)
	}

	if len(result) == 0 {
		result = []string{text}
	}

	return result
}

// applySuggestionOverlay draws the inline suggestion on top of the textarea
// view using the configured ghost style. The first character appears with
// cursor styling (reverse video) so it's visible inside the cursor block.
// Multi-line suggestions are rendered across multiple visual lines.
func (e *editor) applySuggestionOverlay(view string) string {
	lines := strings.Split(view, "\n")
	value := e.textarea.Value()
	promptWidth := runewidth.StringWidth(stripANSI(e.textarea.Prompt))

	// Use LineInfo to get the actual cursor position within soft-wrapped lines
	lineInfo := e.textarea.LineInfo()

	// The cursor's column offset within the current visual line
	textWidth := lineInfo.ColumnOffset

	// Determine the target visual line for the overlay.
	// For soft-wrapped text, we need to find where the cursor actually is.
	var targetLine int

	if strings.HasSuffix(value, "\n") {
		// Cursor is on the line after the last content line.
		// Find the first empty line after content.
		contentLine := -1
		for i := range slices.Backward(lines) {
			if lineHasContent(lines[i], e.textarea.Prompt) {
				contentLine = i
				break
			}
		}
		if contentLine == -1 {
			return view // No content found
		}
		// The cursor line is the one after the content line
		targetLine = contentLine + 1
		if targetLine >= len(lines) {
			// Edge case: cursor line is beyond view (shouldn't happen normally)
			targetLine = contentLine
			textWidth = runewidth.StringWidth(extractLineText(lines[targetLine], e.textarea.Prompt))
		}
	} else {
		// For normal text (including soft-wrapped), use the row offset from LineInfo
		// to find the correct visual line within the viewport.
		// LineInfo().RowOffset gives us how many visual rows down the cursor is
		// from the start of the current logical line.

		// First, find the last visual line with content
		lastContentLine := -1
		for i := range slices.Backward(lines) {
			if lineHasContent(lines[i], e.textarea.Prompt) {
				lastContentLine = i
				break
			}
		}
		if lastContentLine == -1 {
			return view
		}

		// Calculate the target line based on the logical line's row offset
		// For multi-line content, we need to account for previous lines
		logicalLine := e.textarea.Line()
		rowOffset := lineInfo.RowOffset

		// Count how many visual lines come before the current logical line
		visualLinesBeforeCursor := 0
		valueLines := strings.Split(value, "\n")
		for i := 0; i < logicalLine && i < len(valueLines); i++ {
			lineWidth := runewidth.StringWidth(valueLines[i])
			editorWidth := e.textarea.Width()
			if editorWidth > 0 {
				// Each logical line takes at least 1 visual line, plus extra for wrapping
				visualLinesBeforeCursor += 1 + lineWidth/editorWidth
			} else {
				visualLinesBeforeCursor++
			}
		}

		targetLine = visualLinesBeforeCursor + rowOffset

		// Clamp to valid range
		if targetLine >= len(lines) {
			targetLine = lastContentLine
		}
		targetLine = max(targetLine, 0)
	}

	// Use textarea's word-wrap logic to compute how the suggestion would be displayed.
	// This ensures the suggestion wraps at the same points as when the text is accepted.
	wrappedLines := e.computeWrappedLines(e.suggestion, textWidth)

	type overlay struct {
		x, y    int
		content string
	}
	var overlays []overlay

	for i, suggLine := range wrappedLines {
		if suggLine == "" && i > 0 {
			// Empty line in middle of suggestion - skip but keep line count
			continue
		}

		currentY := targetLine + i
		// Note: We intentionally don't skip lines beyond the view; the
		// output is extended to accommodate overlays positioned beyond
		// the base view's boundaries.

		var xOffset int
		if i == 0 {
			// First line starts at cursor position
			xOffset = promptWidth + textWidth
		} else {
			// Subsequent lines start at the prompt position (column 0 after prompt)
			xOffset = promptWidth
		}

		if i == 0 {
			// First line: first character gets cursor styling, rest gets ghost styling
			firstRune, restOfLine := splitFirstRune(suggLine)
			cursorChar := styles.SuggestionCursorStyle.Render(firstRune)

			overlays = append(overlays, overlay{x: xOffset, y: currentY, content: cursorChar})

			if restOfLine != "" {
				ghostRest := styles.SuggestionGhostStyle.Render(restOfLine)
				overlays = append(overlays, overlay{
					x:       xOffset + runewidth.StringWidth(firstRune),
					y:       currentY,
					content: ghostRest,
				})
			}
		} else {
			// Subsequent lines: all ghost styling
			ghostLine := styles.SuggestionGhostStyle.Render(suggLine)
			overlays = append(overlays, overlay{x: xOffset, y: currentY, content: ghostLine})
		}
	}

	if len(overlays) == 0 {
		return view
	}

	// Splice the overlays into the rendered view line by line. This is
	// ANSI-aware string surgery instead of lipgloss canvas compositing, so
	// the editor does not depend on canvas/compositor APIs that differ
	// across lipgloss v2 pre-releases (it keeps the editor embeddable by
	// consumers pinned to other lipgloss snapshots, e.g. Docker Sandboxes).
	outLines := strings.Split(view, "\n")
	for _, ov := range overlays {
		for len(outLines) <= ov.y {
			outLines = append(outLines, "")
		}
		outLines[ov.y] = spliceLine(outLines[ov.y], ov.content, ov.x)
	}
	return strings.Join(outLines, "\n")
}

// spliceLine overwrites line content at column x with overlay, preserving
// ANSI styling on both sides and padding when the line is shorter than x.
func spliceLine(line, overlay string, x int) string {
	left := ansi.Truncate(line, x, "")
	if pad := x - ansi.StringWidth(left); pad > 0 {
		left += strings.Repeat(" ", pad)
	}
	cut := x + ansi.StringWidth(overlay)
	right := ""
	if lineWidth := ansi.StringWidth(line); cut < lineWidth {
		right = ansi.TruncateLeft(line, cut, "")
		// A wide rune straddling the cut is kept whole by TruncateLeft,
		// which would shift the rest of the line one cell to the right.
		// Cut the straddled rune entirely and pad its uncovered cell with
		// a space — what a terminal shows for a half-overwritten cell.
		if extra := ansi.StringWidth(right) - (lineWidth - cut); extra > 0 {
			right = ansi.TruncateLeft(right, extra+1, "")
			right = strings.Repeat(" ", lineWidth-cut-ansi.StringWidth(right)) + right
		}
	}
	return left + overlay + right
}

// splitFirstRune splits a string into its first rune and the rest.
func splitFirstRune(s string) (string, string) {
	if s == "" {
		return "", ""
	}
	runes := []rune(s)
	return string(runes[0]), string(runes[1:])
}

// deleteLastGraphemeCluster removes the last grapheme cluster from the string.
// This handles multi-codepoint characters like emoji sequences correctly.
func deleteLastGraphemeCluster(s string) string {
	if s == "" {
		return s
	}

	// Iterate through grapheme clusters to find where the last one starts
	var lastClusterStart int
	gr := uniseg.NewGraphemes(s)
	for gr.Next() {
		start, _ := gr.Positions()
		lastClusterStart = start
	}

	return s[:lastClusterStart]
}

// refreshSuggestion updates the cached suggestion to reflect the current
// textarea value and available history entries.
func (e *editor) refreshSuggestion() {
	// Don't overwrite completion-managed suggestions with history suggestions.
	if e.currentCompletion != nil {
		return
	}

	e.clearSuggestion()

	current := e.textarea.Value()
	if e.hist == nil || current == "" || !e.isCursorAtEnd() {
		return
	}

	// Only show a suggestion when history has a longer match.
	match := e.hist.LatestMatch(current)
	if len(match) <= len(current) {
		return
	}

	e.suggestion = match[len(current):]
	e.hasSuggestion = true
}

// clearSuggestion removes any pending suggestion.
func (e *editor) clearSuggestion() {
	if !e.hasSuggestion {
		return
	}
	e.hasSuggestion = false
	e.suggestion = ""
}

// isCursorAtEnd returns true if the cursor is at the end of the text.
func (e *editor) isCursorAtEnd() bool {
	value := e.textarea.Value()
	if value == "" {
		return true
	}

	// Check if cursor is on the last logical line
	lines := strings.Split(value, "\n")
	lastLineIdx := len(lines) - 1
	if e.textarea.Line() != lastLineIdx {
		return false
	}

	// Check if cursor is at the end of the last line
	lastLine := lines[lastLineIdx]
	lastLineLen := len([]rune(lastLine))
	lineInfo := e.textarea.LineInfo()

	// For soft-wrapped lines, we need to calculate the total character position
	// from the start of the logical line. CharOffset is relative to the visual line,
	// so we need to add the characters from previous visual rows.
	// StartColumn gives us the character index where the current visual line starts.
	totalCharPos := lineInfo.StartColumn + lineInfo.ColumnOffset

	return totalCharPos >= lastLineLen
}

// AcceptSuggestion applies the current suggestion into the textarea value and
// returns a command to update the completion query, or nil if no suggestion was applied.
func (e *editor) AcceptSuggestion() tea.Cmd {
	if !e.hasSuggestion || e.suggestion == "" {
		return nil
	}

	current := e.textarea.Value()
	e.textarea.SetValue(current + e.suggestion)
	e.textarea.MoveToEnd()

	e.clearSuggestion()

	// Update the completion query to reflect the new editor content
	return e.updateCompletionQuery()
}

func (e *editor) ScrollByWheel(delta int) {
	if delta == 0 {
		return
	}

	steps := delta
	if steps < 0 {
		steps = -steps
		for range steps {
			e.textarea.CursorUp()
		}
		return
	}

	for range steps {
		e.textarea.CursorDown()
	}
}

// resetAndSend prepares a message for sending: processes pending file refs,
// collects attachments, resets editor state, and returns the SendMsg command.
func (e *editor) resetAndSend(content string) tea.Cmd {
	return e.resetAndSendMode(content, false)
}

func (e *editor) resetAndSendMode(content string, followUp bool) tea.Cmd {
	e.tryAddFileRef(e.pendingFileRef)
	e.pendingFileRef = ""
	attachments := e.collectAttachments(content)

	var finalAttachments []messages.Attachment
	var pastes []messages.Attachment

	for _, att := range attachments {
		if att.Content != "" && strings.HasPrefix(att.Name, "paste-") {
			pastes = append(pastes, att)
		} else {
			finalAttachments = append(finalAttachments, att)
		}
	}

	// Sort pastes by name length descending to avoid partial matches
	// e.g., replacing @paste-1 before @paste-10 would corrupt @paste-10.
	slices.SortFunc(pastes, func(a, b messages.Attachment) int {
		return len(b.Name) - len(a.Name)
	})

	for _, att := range pastes {
		content = strings.ReplaceAll(content, "@"+att.Name, att.Content)
	}

	e.textarea.Reset()
	e.userTyped = false
	e.clearSuggestion()
	return core.CmdHandler(messages.SendMsg{Content: content, Attachments: finalAttachments, FollowUp: followUp})
}

// configureNewlineKeybinding sets up the newline keybinding from the
// user-configurable EditorNewline binding (ctrl+j by default), layering
// shift+enter on top whenever the terminal can report it. This keeps the
// historical defaults intact while letting users replace the ctrl+j fallback
// that conflicts with common editor/terminal shortcuts (see issue #1626).
func (e *editor) configureNewlineKeybinding() {
	// Clone so the textarea's keymap never aliases the cached KeyMap slice.
	newlineKeys := slices.Clone(core.GetKeys().EditorNewline.Keys())
	if e.keyboardEnhancementsSupported && !slices.Contains(newlineKeys, "shift+enter") {
		newlineKeys = append([]string{"shift+enter"}, newlineKeys...)
	}
	e.textarea.KeyMap.InsertNewline.SetKeys(newlineKeys...)
	e.textarea.KeyMap.InsertNewline.SetEnabled(true)
}

var (
	editorHistoryUp    = key.NewBinding(key.WithKeys("up"))
	editorHistoryDown  = key.NewBinding(key.WithKeys("down"))
	editorSpace        = key.NewBinding(key.WithKeys("space"))
	editorFollowUp     = key.NewBinding(key.WithKeys("alt+enter"))
	historySearchApply = key.NewBinding(key.WithKeys("enter"))
	historySearchClose = key.NewBinding(key.WithKeys("esc", "ctrl+g"))
)

// Update handles messages and updates the component state
func (e *editor) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	defer e.updateAttachmentBanner()

	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case recordingDotsTickMsg:
		if !e.recording {
			return e, nil
		}
		// Cycle through dot phases: "·", "··", "···"
		e.recordingDotPhase = (e.recordingDotPhase + 1) % 4
		dots := strings.Repeat("·", e.recordingDotPhase)
		if e.recordingDotPhase == 0 {
			dots = ""
		}
		e.textarea.Placeholder = "🎤 Listening" + dots
		cmd := e.tickRecordingDots()
		return e, cmd
	case tea.PasteMsg:
		content, handled := e.handlePaste(msg.Content)
		if handled {
			return e, nil
		}
		// Forward the normalized content explicitly; falling through would
		// hand the textarea the raw message, whose CR/CRLF line endings
		// corrupt the rendered layout.
		msg.Content = content
		var cmd tea.Cmd
		e.textarea, cmd = e.textarea.Update(msg)
		e.refreshSuggestion()
		return e, cmd
	case tea.KeyboardEnhancementsMsg:
		// Track keyboard enhancement support and configure newline keybinding accordingly
		e.keyboardEnhancementsSupported = msg.Flags != 0 || termfeatures.SupportsModifiedEnter(os.Getenv)
		e.configureNewlineKeybinding()
		return e, nil
	case messages.ThemeChangedMsg:
		e.textarea.SetStyles(styles.InputStyle)
		e.searchInput.SetStyles(searchInputStyles())
		return e, nil
	case tea.WindowSizeMsg:
		e.textarea.SetWidth(msg.Width - 2)
		return e, nil

	case tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg:
		var cmd tea.Cmd
		e.textarea, cmd = e.textarea.Update(msg)
		// Give focus to editor on click
		if _, ok := msg.(tea.MouseClickMsg); ok {
			return e, tea.Batch(cmd, e.Focus())
		}
		return e, cmd

	case completion.SelectedMsg:
		if e.argumentPrefix != "" {
			// Argument-mode selection: Value is the FULL command line (e.g.
			// "/toolset-restart github"), not a single word, so it replaces
			// the editor content wholesale rather than splicing a trigger word.
			if msg.AutoSubmit {
				cmd := e.resetAndSend(msg.Value)
				return e, cmd
			}
			e.textarea.SetValue(msg.Value)
			e.textarea.MoveToEnd()
			e.clearSuggestion()
			return e, nil
		}

		if e.currentCompletion == nil {
			return e, nil
		}

		atCompletion := e.currentCompletion.Trigger() == "@" && !strings.HasPrefix(msg.Value, "@paste-")
		triggerWord := e.currentCompletion.Trigger() + e.completionWord
		currentValue := e.textarea.Value()
		idx := strings.LastIndex(currentValue, triggerWord)

		// Handle Execute functions (e.g., "Browse files...")
		// There is an execute function AND you hit enter, or there is an @ directive
		if msg.Execute != nil && (msg.AutoSubmit || atCompletion) {
			if idx >= 0 {
				e.textarea.SetValue(currentValue[:idx] + currentValue[idx+len(triggerWord):])
				e.textarea.MoveToEnd()
			}
			e.clearSuggestion()
			return e, msg.Execute()
		}

		// Handle Auto-Submit items (e.g., commands like "/exit")
		if msg.AutoSubmit && !atCompletion {
			extraText := ""
			if idx >= 0 {
				extraText = currentValue[idx+len(triggerWord):]
			}
			cmd := e.resetAndSend(msg.Value + extraText)
			return e, cmd
		}

		// Insert standard completions (e.g., file paths or text pastes)
		if idx >= 0 {
			newValue := currentValue[:idx] + msg.Value + " " + currentValue[idx+len(triggerWord):]
			e.textarea.SetValue(newValue)
			e.textarea.MoveToEnd()
		}

		// Track valid file references
		if atCompletion {
			if err := e.addFileAttachment(msg.Value); err != nil {
				slog.Warn("failed to add file attachment from completion", "value", msg.Value, "error", err)
			}
		}

		e.clearSuggestion()
		return e, nil
	case completion.ClosedMsg:
		e.completionWord = ""
		e.currentCompletion = nil
		e.argumentPrefix = ""
		e.refreshSuggestion()
		// Reset file loading state
		e.fileLoadStarted = false
		e.fileFullLoadStarted = false
		if e.fileLoadCancel != nil {
			e.fileLoadCancel()
			e.fileLoadCancel = nil
		}
		return e, e.textarea.Focus()

	case fileLoadResultMsg:
		// Ignore stale results from older loads.
		if msg.loadID != e.fileLoadID {
			return e, nil
		}

		// Always stop the loading indicator for the active load, even if it was cancelled/errored.
		if msg.items == nil {
			return e, core.CmdHandler(completion.SetLoadingMsg{Loading: false})
		}
		// For full load, replace items (keeping pinned); for initial, append
		var itemsCmd tea.Cmd
		if msg.isFullLoad {
			itemsCmd = core.CmdHandler(completion.ReplaceItemsMsg{Items: msg.items})
		} else {
			itemsCmd = core.CmdHandler(completion.AppendItemsMsg{Items: msg.items})
		}
		return e, tea.Batch(
			core.CmdHandler(completion.SetLoadingMsg{Loading: false}),
			itemsCmd,
		)
	case completion.SelectionChangedMsg:
		// Show the selected completion item as a suggestion in the editor.
		e.clearSuggestion()
		if msg.Value != "" && e.currentCompletion != nil {
			currentText := e.textarea.Value()
			if strings.HasPrefix(msg.Value, currentText) {
				e.suggestion = msg.Value[len(currentText):]
				e.hasSuggestion = e.suggestion != ""
			}
		}
		return e, nil
	case tea.KeyPressMsg:
		if e.historySearch.active {
			return e.handleHistorySearchKey(msg)
		}

		if key.Matches(msg, e.textarea.KeyMap.Paste) {
			return e.handleClipboardPaste()
		}

		// Handle backspace with grapheme cluster awareness.
		// The default textarea.Model only deletes a single rune, which breaks
		// multi-codepoint characters like emoji (e.g., ⚠️ = U+26A0 + U+FE0F).
		if key.Matches(msg, e.textarea.KeyMap.DeleteCharacterBackward) {
			return e.handleGraphemeBackspace()
		}

		// Alt+Enter submits an end-of-turn follow-up. It is handled before the
		// configurable newline binding so the two delivery modes are always
		// available independently.
		if key.Matches(msg, editorFollowUp) {
			if !e.textarea.Focused() {
				return e, nil
			}
			if value := e.textarea.Value(); value != "" {
				cmd := e.resetAndSendMode(value, true)
				return e, cmd
			}
			return e, nil
		}

		// Handle send/newline keys (both user-configurable, see issue #1626):
		// - EditorSend (enter by default): submit the current input.
		// - EditorNewline (ctrl+j by default) / shift+enter: insert a newline,
		//   handled by the textarea's InsertNewline binding.
		isSend := key.Matches(msg, core.GetKeys().EditorSend)
		if isSend || key.Matches(msg, e.textarea.KeyMap.InsertNewline) {
			if !e.textarea.Focused() {
				return e, nil
			}

			// Let textarea process the key - it handles newlines via InsertNewline binding
			prev := e.textarea.Value()
			e.textarea, _ = e.textarea.Update(msg)
			value := e.textarea.Value()

			// If textarea inserted a newline, just refresh and return
			if value != prev && !isSend {
				e.refreshSuggestion()
				return e, nil
			}

			// If the send key also inserted a newline, submit the previous value
			if value != prev && isSend {
				if prev != "" {
					e.textarea.SetValue(prev)
					e.textarea.MoveToEnd()
					cmd := e.resetAndSend(prev)
					return e, cmd
				}
				return e, nil
			}

			// Normal send: send current value
			if value != "" {
				cmd := e.resetAndSend(value)
				return e, cmd
			}

			return e, nil
		}

		// Handle other special keys
		switch {
		case key.Matches(msg, editorHistoryUp):
			// Only navigate history if the user hasn't manually typed content
			if !e.userTyped {
				e.textarea.SetValue(e.hist.Previous())
				e.textarea.MoveToEnd()
				e.refreshSuggestion()
				return e, nil
			}
			// Otherwise, let the textarea handle cursor navigation
		case key.Matches(msg, editorHistoryDown):
			// Only navigate history if the user hasn't manually typed content
			if !e.userTyped {
				e.textarea.SetValue(e.hist.Next())
				e.textarea.MoveToEnd()
				e.refreshSuggestion()
				return e, nil
			}
			// Otherwise, let the textarea handle cursor navigation
		default:
			for _, completion := range e.completions {
				if msg.String() == completion.Trigger() {
					if completion.RequiresEmptyEditor() && e.textarea.Value() != "" {
						continue
					}
					cmds = append(cmds, e.startCompletion(completion))
				}
			}
		}
	}

	prevValue := e.textarea.Value()
	var cmd tea.Cmd
	e.textarea, cmd = e.textarea.Update(msg)
	cmds = append(cmds, cmd)

	// If the value changed due to user input (not history navigation), mark as user typed
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		// Check if content changed and it wasn't a history navigation key
		if e.textarea.Value() != prevValue && !key.Matches(keyMsg, editorHistoryUp, editorHistoryDown) {
			e.userTyped = true
		}

		// Also check if textarea became empty - reset userTyped flag
		if e.textarea.Value() == "" {
			e.userTyped = false
		}

		currentWord := e.textarea.Word()

		// Track manual @filepath refs - only runs when we're in/leaving an @ word
		if e.pendingFileRef != "" && currentWord != e.pendingFileRef {
			// Left the @ word - try to add it as file ref
			e.tryAddFileRef(e.pendingFileRef)
			e.pendingFileRef = ""
		}
		if e.pendingFileRef == "" && strings.HasPrefix(currentWord, "@") && len(currentWord) > 1 {
			// Entered an @ word - start tracking
			e.pendingFileRef = currentWord
		} else if e.pendingFileRef != "" && strings.HasPrefix(currentWord, "@") {
			// Still in @ word but it changed (user typing more) - update tracking
			e.pendingFileRef = currentWord
		}

		if key.Matches(keyMsg, editorSpace) {
			e.currentCompletion = nil
		}

		cmds = append(cmds, e.updateCompletionQuery())
	}

	e.refreshSuggestion()

	return e, tea.Batch(cmds...)
}

func (e *editor) handleClipboardPaste() (layout.Model, tea.Cmd) {
	content, err := clipboard.ReadAll()
	if err != nil {
		slog.Warn("failed to read clipboard", "error", err)
		return e, nil
	}

	// Insert the normalized content returned by handlePaste so the inserted
	// text always matches what was classified.
	normalized, handled := e.handlePaste(content)
	if !handled {
		e.textarea.InsertString(normalized)
	}
	return e, textarea.Blink
}

// handleGraphemeBackspace implements backspace with grapheme cluster awareness.
// It removes the entire last grapheme cluster, not just the last rune.
// This fixes deletion of multi-codepoint characters like emoji sequences.
func (e *editor) handleGraphemeBackspace() (layout.Model, tea.Cmd) {
	value := e.textarea.Value()
	if value == "" {
		return e, nil
	}

	// Get cursor position info
	lines := strings.Split(value, "\n")
	currentLine := e.textarea.Line()
	lineInfo := e.textarea.LineInfo()

	// CharOffset within the current visual line segment
	colPos := lineInfo.CharOffset + lineInfo.StartColumn

	if currentLine < 0 || currentLine >= len(lines) {
		return e, nil
	}

	if colPos == 0 && currentLine > 0 {
		// At beginning of line but not first line - let textarea handle line merge
		var cmd tea.Cmd
		e.textarea, cmd = e.textarea.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
		e.refreshSuggestion()
		return e, tea.Batch(cmd, e.updateCompletionQuery())
	}

	if colPos == 0 {
		// At beginning of first line - nothing to delete
		return e, nil
	}

	// Delete the last grapheme cluster from the text before the cursor
	currentLineText := lines[currentLine]

	// Convert column position (based on display width) to rune position
	runePos := 0
	width := 0
	for _, r := range currentLineText {
		if width >= colPos {
			break
		}
		width += runewidth.RuneWidth(r)
		runePos++
	}

	// Text before cursor
	runes := []rune(currentLineText)
	if runePos > len(runes) {
		runePos = len(runes)
	}
	beforeCursor := string(runes[:runePos])
	afterCursor := string(runes[runePos:])

	// Delete the last grapheme cluster from text before cursor
	newBeforeCursor := deleteLastGraphemeCluster(beforeCursor)

	// Rebuild the line
	lines[currentLine] = newBeforeCursor + afterCursor
	newValue := strings.Join(lines, "\n")

	// Calculate new cursor column position within the current line
	newCol := len([]rune(newBeforeCursor))

	// Build text before cursor position (all lines before current + new before cursor)
	var beforeParts []string
	for i := range currentLine {
		beforeParts = append(beforeParts, lines[i])
	}
	beforeParts = append(beforeParts, newBeforeCursor)
	textBeforeCursor := strings.Join(beforeParts, "\n")

	// Build text after cursor position (after cursor on current line + remaining lines)
	var textAfterCursorSb strings.Builder
	textAfterCursorSb.WriteString(afterCursor)
	for i := currentLine + 1; i < len(lines); i++ {
		textAfterCursorSb.WriteByte('\n')
		textAfterCursorSb.WriteString(lines[i])
	}
	textAfterCursor := textAfterCursorSb.String()

	// Set the text before cursor and move to end
	e.textarea.SetValue(textBeforeCursor)
	e.textarea.MoveToEnd()

	// Now insert the text after cursor - this positions cursor correctly
	if textAfterCursor != "" {
		e.textarea.SetValue(newValue)
		e.textarea.MoveToBegin()

		// Keep calling CursorDown until we're on the target logical line
		for e.textarea.Line() < currentLine {
			e.textarea.CursorDown()
		}

		e.textarea.SetCursorColumn(newCol)
	}

	e.refreshSuggestion()
	return e, tea.Batch(textarea.Blink, e.updateCompletionQuery())
}

// updateCompletionQuery sends the appropriate completion message based on current editor state.
// It returns a command that either updates the completion query or closes the completion popup.
func (e *editor) updateCompletionQuery() tea.Cmd {
	if e.argumentPrefix != "" {
		value := e.textarea.Value()
		if query, ok := strings.CutPrefix(value, e.argumentPrefix); ok {
			return core.CmdHandler(completion.QueryMsg{Query: query})
		}
		// Value no longer starts with the argument prefix (e.g. the command
		// name itself was edited) - end the argument-completion session.
		e.argumentPrefix = ""
		e.currentCompletion = nil
		e.completionWord = ""
		e.clearSuggestion()
		return core.CmdHandler(completion.CloseMsg{})
	}

	currentWord := e.textarea.Word()

	if e.currentCompletion != nil && strings.HasPrefix(currentWord, e.currentCompletion.Trigger()) {
		e.completionWord = strings.TrimPrefix(currentWord, e.currentCompletion.Trigger())

		// For @ completion, start full file loading when user starts typing (if not already started)
		var loadCmd tea.Cmd
		if e.currentCompletion.Trigger() == "@" && e.completionWord != "" && !e.fileFullLoadStarted {
			loadCmd = e.startFullFileLoad()
		}

		queryCmd := core.CmdHandler(completion.QueryMsg{Query: e.completionWord})
		if loadCmd != nil {
			return tea.Batch(queryCmd, loadCmd)
		}
		return queryCmd
	}

	e.completionWord = ""
	e.clearSuggestion()
	return core.CmdHandler(completion.CloseMsg{})
}

// startFullFileLoad starts full background file loading and returns a command that will
// emit a fileLoadResultMsg when complete. This is triggered when the user starts typing.
func (e *editor) startFullFileLoad() tea.Cmd {
	e.fileFullLoadStarted = true
	e.fileLoadID++
	loadID := e.fileLoadID

	// Cancel any previous load
	if e.fileLoadCancel != nil {
		e.fileLoadCancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	e.fileLoadCancel = cancel

	// Find the file completion that supports async loading
	var asyncLoader completions.AsyncLoader
	for _, c := range e.completions {
		if c.Trigger() == "@" {
			if al, ok := c.(completions.AsyncLoader); ok {
				asyncLoader = al
				break
			}
		}
	}

	if asyncLoader == nil {
		return nil
	}

	// Set loading state
	loadingCmd := core.CmdHandler(completion.SetLoadingMsg{Loading: true})

	// Start full async load
	asyncCmd := func() tea.Msg {
		ch := asyncLoader.LoadItemsAsync(ctx)
		items := <-ch
		return fileLoadResultMsg{loadID: loadID, items: items, isFullLoad: true}
	}

	return tea.Batch(loadingCmd, asyncCmd)
}

func (e *editor) startCompletion(c completions.Completion) tea.Cmd {
	// For @ trigger, open instantly with paste items + "Browse files…" and start async file loading
	if c.Trigger() == "@" {
		items := e.getPasteCompletionItems()
		// Add "Browse files…" action that opens the file picker dialog
		items = append(items, completion.Item{
			Label:       "Browse files…",
			Description: "Open file picker",
			Value:       "", // No value to insert
			Execute: func() tea.Cmd {
				return core.CmdHandler(messages.AttachFileMsg{FilePath: ""})
			},
			Pinned: true,
		})

		openCmd := e.openCompletion(c, items)

		// Start initial shallow file loading immediately
		loadCmd := e.startInitialFileLoad()

		return tea.Batch(openCmd, loadCmd)
	}

	return e.openCompletion(c, c.Items())
}

// openCompletion activates c as the current completion source and opens the
// popup with the given items. Shared by name-completion (startCompletion)
// and argument-mode (TryStartArgumentCompletion), which supply different
// item sets for the same completion source.
func (e *editor) openCompletion(c completions.Completion, items []completion.Item) tea.Cmd {
	e.currentCompletion = c
	return core.CmdHandler(completion.OpenMsg{
		Items:     items,
		MatchMode: c.MatchMode(),
	})
}

// TryStartArgumentCompletion implements [Editor]. See the interface doc for
// the full contract.
func (e *editor) TryStartArgumentCompletion() tea.Cmd {
	value := e.textarea.Value()
	// Require a space so a command name that's still being typed (and whose
	// name-completion popup narrows it) never gets reinterpreted as an
	// argument-completion trigger.
	if !strings.Contains(value, " ") {
		return nil
	}

	for _, c := range e.completions {
		argCompleter, ok := c.(completions.ArgumentCompleter)
		if !ok {
			continue
		}

		items, matched := argCompleter.ArgumentItems(value)
		if !matched || len(items) == 0 {
			continue
		}

		slashCommand, query, _ := strings.Cut(value, " ")
		e.argumentPrefix = slashCommand + " "

		// Sequenced (not batched) so the popup opens with the full item set
		// before the query narrows it, regardless of scheduling order.
		return tea.Sequence(
			e.openCompletion(c, items),
			core.CmdHandler(completion.QueryMsg{Query: query}),
		)
	}

	return nil
}

// startInitialFileLoad starts a shallow file scan for immediate display.
// It loads ~100 files from 2 levels deep for a snappy initial UX.
func (e *editor) startInitialFileLoad() tea.Cmd {
	e.fileLoadStarted = true
	e.fileLoadID++
	loadID := e.fileLoadID

	// Cancel any previous load
	if e.fileLoadCancel != nil {
		e.fileLoadCancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	e.fileLoadCancel = cancel

	// Find the file completion that supports async loading
	var asyncLoader completions.AsyncLoader
	for _, c := range e.completions {
		if c.Trigger() == "@" {
			if al, ok := c.(completions.AsyncLoader); ok {
				asyncLoader = al
				break
			}
		}
	}

	if asyncLoader == nil {
		return nil
	}

	// Set loading state
	loadingCmd := core.CmdHandler(completion.SetLoadingMsg{Loading: true})

	// Start initial shallow load
	asyncCmd := func() tea.Msg {
		ch := asyncLoader.LoadInitialItemsAsync(ctx)
		items := <-ch
		return fileLoadResultMsg{loadID: loadID, items: items, isFullLoad: false}
	}

	return tea.Batch(loadingCmd, asyncCmd)
}

// getPasteCompletionItems returns completion items for paste attachments only.
func (e *editor) getPasteCompletionItems() []completion.Item {
	var items []completion.Item
	for _, att := range e.attachments {
		if !att.isTemp {
			continue // Only show pastes, not file refs
		}
		name := strings.TrimPrefix(att.placeholder, "@")
		items = append(items, completion.Item{
			Label:       name,
			Description: units.HumanSize(float64(att.sizeBytes)),
			Value:       att.placeholder,
			Pinned:      true,
		})
	}
	return items
}

// View renders the component
func (e *editor) View() string {
	view := e.textarea.View()

	if e.textarea.Focused() && e.hasSuggestion && e.suggestion != "" {
		view = e.applySuggestionOverlay(view)
	}

	bannerView := e.banner.View()
	if bannerView != "" {
		view = lipgloss.JoinVertical(lipgloss.Left, bannerView, view)
	}

	if e.historySearch.active {
		view = lipgloss.JoinVertical(lipgloss.Left, view, e.searchInput.View())
	}

	return styles.RenderComposite(styles.EditorStyle.MarginBottom(1), view)
}

// SetSize sets the dimensions of the component
func (e *editor) SetSize(width, height int) tea.Cmd {
	e.width = width
	e.height = max(height, 1)

	e.textarea.SetWidth(max(width, 10))
	e.searchInput.SetWidth(max(width, 10))
	e.updateTextareaHeight()

	return nil
}

func (e *editor) updateTextareaHeight() {
	available := e.height
	if e.banner != nil {
		available -= e.banner.Height()
	}
	if e.historySearch.active {
		available--
	}

	available = max(available, 1)

	e.textarea.SetHeight(available)
}

// BannerHeight returns the current height of the attachment banner (0 if hidden)
func (e *editor) BannerHeight() int {
	if e.banner == nil {
		return 0
	}
	return e.banner.Height()
}

// GetSize returns the rendered dimensions including EditorStyle padding.
func (e *editor) GetSize() (width, height int) {
	return e.width + styles.EditorStyle.GetHorizontalFrameSize(),
		e.height + styles.EditorStyle.GetVerticalFrameSize()
}

// AttachmentAt returns preview information for the attachment rendered at the given X position.
func (e *editor) AttachmentAt(x int) (AttachmentPreview, bool) {
	if e.banner == nil || e.banner.Height() == 0 {
		return AttachmentPreview{}, false
	}

	item, ok := e.banner.HitTest(x)
	if !ok {
		return AttachmentPreview{}, false
	}

	for _, att := range e.attachments {
		if att.placeholder != item.placeholder {
			continue
		}

		data, err := os.ReadFile(att.path)
		if err != nil {
			slog.Warn("failed to read attachment preview", "path", att.path, "error", err)
			return AttachmentPreview{}, false
		}

		return AttachmentPreview{
			Title:   item.label,
			Content: string(data),
		}, true
	}

	return AttachmentPreview{}, false
}

// Focus gives focus to the component
func (e *editor) Focus() tea.Cmd {
	return e.textarea.Focus()
}

// Blur removes focus from the component
func (e *editor) Blur() tea.Cmd {
	e.textarea.Blur()
	e.clearSuggestion()
	return nil
}

func (e *editor) SetWorking(working bool) tea.Cmd {
	e.working = working
	return nil
}

// Value returns the current editor content
func (e *editor) Value() string {
	return e.textarea.Value()
}

// SetValue updates the editor content and moves cursor to end
func (e *editor) SetValue(content string) {
	e.textarea.SetValue(content)
	e.textarea.MoveToEnd()
	e.userTyped = content != ""
	e.refreshSuggestion()
}

// InsertText inserts text at the current cursor position
func (e *editor) InsertText(text string) {
	e.textarea.InsertString(text)
	e.userTyped = true
	e.refreshSuggestion()
}

// AttachFile adds a file as an attachment and inserts @filepath into the editor
func (e *editor) AttachFile(filePath string) error {
	placeholder := "@" + filePath
	if err := e.addFileAttachment(placeholder); err != nil {
		return fmt.Errorf("failed to attach %s: %w", filePath, err)
	}
	currentValue := e.textarea.Value()
	e.textarea.SetValue(currentValue + placeholder + " ")
	e.textarea.MoveToEnd()
	e.userTyped = true
	e.updateAttachmentBanner()
	return nil
}

// tryAddFileRef checks if word is a valid @filepath and adds it as attachment.
// Called when cursor leaves a word to detect manually-typed file references.
func (e *editor) tryAddFileRef(word string) {
	// Must start with @ and look like a path (contains / or .)
	if !strings.HasPrefix(word, "@") || len(word) < 2 {
		return
	}

	// Don't track paste placeholders as file refs
	if strings.HasPrefix(word, "@paste-") {
		return
	}

	path := word[1:] // strip @
	if !strings.ContainsAny(path, "/.") {
		return // not a path-like reference (e.g., @username)
	}

	if err := e.addFileAttachment(word); err != nil {
		slog.Debug("speculative file ref not valid", "word", word, "error", err)
	}
}

// addFileAttachment adds a file reference as an attachment if valid.
// The path is resolved to an absolute path so downstream consumers
// (e.g. processFileAttachment) always receive a fully qualified path.
func (e *editor) addFileAttachment(placeholder string) error {
	path := strings.TrimPrefix(placeholder, "@")

	// Resolve to absolute path so the attachment carries a fully qualified
	// path regardless of the working directory at send time.
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("cannot resolve path %s: %w", path, err)
	}

	info, err := validateFilePath(absPath)
	if err != nil {
		return fmt.Errorf("invalid file path %s: %w", absPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("path is a directory: %s", absPath)
	}

	const maxFileSize = 5 * 1024 * 1024
	if info.Size() >= maxFileSize {
		return fmt.Errorf("file too large: %s (%s)", absPath, units.HumanSize(float64(info.Size())))
	}

	// Avoid duplicates
	for _, att := range e.attachments {
		if att.placeholder == placeholder {
			return nil
		}
	}

	e.attachments = append(e.attachments, attachment{
		path:        absPath,
		placeholder: placeholder,
		label:       fmt.Sprintf("%s (%s)", filepath.Base(absPath), units.HumanSize(float64(info.Size()))),
		sizeBytes:   int(info.Size()),
		isTemp:      false,
	})
	return nil
}

// collectAttachments returns structured attachments for all items referenced in
// content. For paste attachments the content is read into memory (the backing
// temp file is removed). For file-reference attachments the path is preserved
// so the consumer can read and classify the file (e.g. detect MIME type).
// Unreferenced attachments are cleaned up.
func (e *editor) collectAttachments(content string) []messages.Attachment {
	if len(e.attachments) == 0 {
		return nil
	}

	var result []messages.Attachment
	for _, att := range e.attachments {
		if !strings.Contains(content, att.placeholder) {
			if att.isTemp {
				_ = os.Remove(att.path)
			}
			continue
		}

		if att.isTemp {
			// Paste attachment: read into memory and remove the temp file.
			data, err := os.ReadFile(att.path)
			_ = os.Remove(att.path)
			if err != nil {
				slog.Warn("failed to read paste attachment", "path", att.path, "error", err)
				continue
			}
			result = append(result, messages.Attachment{
				Name:    strings.TrimPrefix(att.placeholder, "@"),
				Content: string(data),
			})
		} else {
			// File-reference attachment: keep the path for later processing.
			result = append(result, messages.Attachment{
				Name:     filepath.Base(att.path),
				FilePath: att.path,
			})
		}
	}
	e.attachments = nil

	return result
}

// Cleanup removes any temporary paste files that haven't been sent yet.
func (e *editor) Cleanup() {
	for _, att := range e.attachments {
		if att.isTemp {
			_ = os.Remove(att.path)
		}
	}
	e.attachments = nil
}

// SetRecording sets the recording mode which shows animated dots as the cursor.
// When recording is enabled, the placeholder changes to animated dots.
func (e *editor) SetRecording(recording bool) tea.Cmd {
	e.recording = recording
	if recording {
		e.recordingDotPhase = 0
		e.textarea.Placeholder = "🎤 Listening"
		return e.tickRecordingDots()
	}
	e.textarea.Placeholder = e.placeholder
	return nil
}

// recordingDotsTickMsg is sent periodically to animate the recording dots
type recordingDotsTickMsg struct{}

// tickRecordingDots returns a command that ticks the recording dots animation
func (e *editor) tickRecordingDots() tea.Cmd {
	return tea.Tick(400*time.Millisecond, func(time.Time) tea.Msg {
		return recordingDotsTickMsg{}
	})
}

// IsRecording returns true if the editor is in recording mode
func (e *editor) IsRecording() bool {
	return e.recording
}

// IsHistorySearchActive returns true if the editor is in history search mode
func (e *editor) IsHistorySearchActive() bool {
	return e.historySearch.active
}

// SendContent triggers sending the current editor content
func (e *editor) SendContent() tea.Cmd {
	value := e.textarea.Value()
	if value == "" {
		return nil
	}
	return e.resetAndSend(value)
}

// normalizePasteNewlines converts CRLF and bare CR line endings to LF.
// Pasted terminal output (e.g. Docker BuildKit logs) can separate records
// with bare CRs, which would defeat line-based classification and let raw
// CRs reach the textarea.
func normalizePasteNewlines(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	return strings.ReplaceAll(content, "\r", "\n")
}

// handlePaste processes pasted content: file paths become attachments and
// large text is buffered to a temp file attachment. It returns the
// line-ending-normalized content and whether the paste was fully handled.
// When handled is false the caller must insert the returned content, which
// guarantees classification and insertion agree on the same string.
func (e *editor) handlePaste(content string) (normalized string, handled bool) {
	content = normalizePasteNewlines(content)

	// First, try to parse as file paths (drag-and-drop)
	filePaths := ParsePastedFiles(content)
	if len(filePaths) > 0 {
		var attached int
		for _, path := range filePaths {
			if !IsSupportedFileType(path) {
				break
			}
			if err := e.AttachFile(path); err != nil {
				slog.Debug("paste path not attachable, treating as text", "path", path, "error", err)
				break
			}
			attached++
		}
		if attached == len(filePaths) {
			return content, true
		}
		// Not all files could be attached; undo partial attachments and fall through to text paste
		e.removeLastNAttachments(attached)
	}

	// Not file paths, handle as text paste
	// Count lines (newlines + 1 for content without trailing newline)
	lines := strings.Count(content, "\n") + 1
	if strings.HasSuffix(content, "\n") {
		lines-- // Don't count trailing newline as extra line
	}

	// Allow inline if within both limits
	if lines <= maxInlinePasteLines && len(content) <= maxInlinePasteChars {
		return content, false
	}

	e.pasteCounter++
	att, err := createPasteAttachment(content, e.pasteCounter)
	if err != nil {
		slog.Warn("failed to buffer paste", "error", err)
		// Still report handled to prevent the large paste from falling
		// through to textarea.Update(), which would block the UI for seconds.
		return content, true
	}

	e.textarea.InsertString(att.placeholder)
	e.attachments = append(e.attachments, att)

	return content, true
}

// removeLastNAttachments removes the last n non-temp attachments and their
// placeholder text from the textarea. Used to roll back partial file-drop
// attachments when not all files in a paste are valid.
func (e *editor) removeLastNAttachments(n int) {
	if n <= 0 {
		return
	}
	value := e.textarea.Value()
	removed := 0
	for i := len(e.attachments) - 1; i >= 0 && removed < n; i-- {
		if !e.attachments[i].isTemp {
			// Strip the placeholder text ("@/path/file.png ") that AttachFile inserted
			value = strings.Replace(value, e.attachments[i].placeholder+" ", "", 1)
			e.attachments = slices.Delete(e.attachments, i, i+1)
			removed++
		}
	}
	e.textarea.SetValue(value)
	e.textarea.MoveToEnd()
}

func (e *editor) updateAttachmentBanner() {
	if e.banner == nil {
		return
	}

	value := e.textarea.Value()
	var items []bannerItem

	for _, att := range e.attachments {
		if strings.Contains(value, att.placeholder) {
			items = append(items, bannerItem{
				label:       att.label,
				placeholder: att.placeholder,
			})
		}
	}

	e.banner.SetItems(items)
	e.updateTextareaHeight()
}

func createPasteAttachment(content string, num int) (attachment, error) {
	pasteDir := filepath.Join(paths.GetDataDir(), "pastes")
	if err := os.MkdirAll(pasteDir, 0o700); err != nil {
		return attachment{}, fmt.Errorf("create paste dir: %w", err)
	}

	file, err := os.CreateTemp(pasteDir, "paste-*.txt")
	if err != nil {
		return attachment{}, fmt.Errorf("create paste file: %w", err)
	}
	defer file.Close()

	if _, err := file.WriteString(content); err != nil {
		return attachment{}, fmt.Errorf("write paste file: %w", err)
	}

	displayName := fmt.Sprintf("paste-%d", num)
	return attachment{
		path:        file.Name(),
		placeholder: "@" + displayName,
		label:       fmt.Sprintf("%s (%s)", displayName, units.HumanSize(float64(len(content)))),
		sizeBytes:   len(content),
		isTemp:      true,
	}, nil
}

func (e *editor) EnterHistorySearch() (layout.Model, tea.Cmd) {
	e.historySearch = historySearchState{
		active:                   true,
		origTextValue:            e.textarea.Value(),
		origTextPlaceholderValue: e.textarea.Placeholder,
		matchIndex:               -1,
	}

	e.searchInput.SetValue("")
	e.textarea.SetValue("")
	e.textarea.Placeholder = ""
	e.textarea.Blur()
	e.clearSuggestion()
	return e, tea.Batch(
		e.searchInput.Focus(),
		core.CmdHandler(completion.CloseMsg{}),
	)
}

func (e *editor) handleHistorySearchKey(msg tea.KeyPressMsg) (layout.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, e.searchInput.KeyMap.PrevSuggestion):
		e.cycleMatch(e.hist.FindPrevContains, len(e.hist.Messages))
		return e, nil

	case key.Matches(msg, e.searchInput.KeyMap.NextSuggestion):
		e.cycleMatch(e.hist.FindNextContains, -1)
		return e, nil

	case key.Matches(msg, historySearchApply):
		value := e.textarea.Value()
		matchIdx := e.historySearch.matchIndex
		cmd := e.exitHistorySearch()
		if value != "" {
			e.textarea.SetValue(value)
			e.textarea.MoveToEnd()
			if matchIdx >= 0 {
				e.hist.SetCurrent(matchIdx)
			}
			e.userTyped = false
		}
		e.refreshSuggestion()
		return e, tea.Batch(cmd, core.CmdHandler(completion.CloseMsg{}))

	case key.Matches(msg, historySearchClose):
		cmd := e.exitHistorySearch()
		e.refreshSuggestion()
		return e, tea.Batch(cmd, core.CmdHandler(completion.CloseMsg{}))
	}

	var cmd tea.Cmd
	e.searchInput, cmd = e.searchInput.Update(msg)

	newQuery := e.searchInput.Value()
	if newQuery != e.historySearch.query {
		e.historySearch.query = newQuery
		e.historySearchComputeMatch()
	}

	return e, cmd
}

// cycleMatch searches history using findFn starting from the current match.
// If no match is found, it wraps around using wrapFrom as the starting point.
func (e *editor) cycleMatch(findFn func(string, int) (string, int, bool), wrapFrom int) {
	if e.historySearch.matchIndex < 0 {
		return
	}
	m, idx, ok := findFn(e.historySearch.query, e.historySearch.matchIndex)
	if !ok {
		m, idx, ok = findFn(e.historySearch.query, wrapFrom)
	}
	if ok {
		e.historySearch.match = m
		e.historySearch.matchIndex = idx
		e.historySearch.failing = false
		e.textarea.SetValue(m)
		e.textarea.MoveToEnd()
	}
}

func (e *editor) historySearchComputeMatch() {
	if e.historySearch.query == "" {
		e.historySearch.match = ""
		e.historySearch.matchIndex = -1
		e.historySearch.failing = false
		e.textarea.SetValue("")
		e.textarea.Placeholder = ""
		return
	}

	m, idx, ok := e.hist.FindPrevContains(e.historySearch.query, len(e.hist.Messages))
	if ok {
		e.historySearch.match = m
		e.historySearch.matchIndex = idx
		e.historySearch.failing = false
		e.textarea.SetValue(m)
		e.textarea.MoveToEnd()
	} else {
		e.historySearch.failing = true
		e.historySearch.match = ""
		e.historySearch.matchIndex = -1
		e.textarea.SetValue("")
		e.textarea.Placeholder = "No matching entry in history"
	}
}

func (e *editor) exitHistorySearch() tea.Cmd {
	e.textarea.SetValue(e.historySearch.origTextValue)
	e.textarea.Placeholder = e.historySearch.origTextPlaceholderValue
	e.historySearch = historySearchState{matchIndex: -1}
	return e.textarea.Focus()
}
