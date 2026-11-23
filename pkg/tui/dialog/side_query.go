package dialog

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// SideQueryRunner executes a /btw side question against context captured
// when the request started, streaming answer chunks through emit. It may be
// called again on retry; each call is an independent request.
type SideQueryRunner func(context.Context, string, func(string)) error

type sideQueryResult struct {
	text string
	err  error
	done bool
}

// SideQueryUpdateMsg carries a streamed /btw answer chunk or terminal result
// back to the owning dialog. It is Broadcastable so the dialog keeps
// receiving updates even when covered by another dialog (e.g. a tool
// confirmation the main agent raised while /btw streams underneath).
type SideQueryUpdateMsg struct {
	owner      *sideQueryDialog
	generation int
	result     sideQueryResult
}

// BroadcastToDialogs implements Broadcastable.
func (SideQueryUpdateMsg) BroadcastToDialogs() {}

type sideQueryTickMsg struct{ owner *sideQueryDialog }

// BroadcastToDialogs implements Broadcastable.
func (sideQueryTickMsg) BroadcastToDialogs() {}

type sideQueryKeyMap struct {
	Close, Ask, Retry, Copy key.Binding
}

func defaultSideQueryKeyMap() sideQueryKeyMap {
	return sideQueryKeyMap{
		Close: key.NewBinding(key.WithKeys("esc"), key.WithHelp("Esc", "close")),
		Ask:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "ask")),
		Retry: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "retry")),
		Copy:  key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy")),
	}
}

type sideQueryDialog struct {
	BaseDialog

	ctx        context.Context //nolint:containedctx // Dialog owns the side request's cancellation scope.
	cancel     context.CancelFunc
	run        SideQueryRunner
	status     func() string
	input      textinput.Model
	keyMap     sideQueryKeyMap
	question   string
	answer     string
	err        error
	running    bool
	closed     bool
	generation int
	updates    <-chan sideQueryResult
	scrollview *scrollview.Model
	frame      int
}

// NewSideQueryDialog creates the /btw dialog. When question is empty, the
// dialog opens a text input first; otherwise it starts immediately. run is
// invoked at most once concurrently and may be called again after a
// failure (retry); status reports the main agent's state for display only.
func NewSideQueryDialog(ctx context.Context, question string, run SideQueryRunner, status func() string) Dialog {
	input := textinput.New()
	input.Placeholder = "Ask a side question…"
	input.SetVirtualCursor(true)
	input.CharLimit = 8192
	input.Focus()
	return &sideQueryDialog{
		ctx:        ctx,
		question:   strings.TrimSpace(question),
		input:      input,
		run:        run,
		status:     status,
		keyMap:     defaultSideQueryKeyMap(),
		scrollview: scrollview.New(scrollview.WithKeyMap(scrollview.ReadOnlyScrollKeyMap()), scrollview.WithReserveScrollbarSpace(true)),
	}
}

func (d *sideQueryDialog) Init() tea.Cmd {
	if d.question != "" {
		return d.start()
	}
	return textinput.Blink
}

// Close also handles programmatic removal, not just Escape.
func (d *sideQueryDialog) Close() {
	d.closed = true
	if d.cancel != nil {
		d.cancel()
	}
}

func (d *sideQueryDialog) start() tea.Cmd {
	if d.closed || d.running {
		return nil
	}
	d.input.Blur()
	d.running, d.answer, d.err = true, "", nil
	d.generation++
	ctx, cancel := context.WithCancel(d.ctx)
	d.cancel = cancel
	updates := make(chan sideQueryResult, 32)
	d.updates = updates
	run, question := d.run, d.question
	work := func() tea.Msg {
		defer close(updates)
		defer cancel()
		emit := func(text string) {
			select {
			case updates <- sideQueryResult{text: text}:
			case <-ctx.Done():
			}
		}
		err := run(ctx, question, emit)
		select {
		case updates <- sideQueryResult{err: err, done: true}:
		case <-ctx.Done():
		}
		return nil
	}
	return tea.Batch(work, d.wait(), d.tick())
}

func (d *sideQueryDialog) wait() tea.Cmd {
	updates, generation, owner := d.updates, d.generation, d
	return func() tea.Msg {
		result, ok := <-updates
		if !ok {
			result = sideQueryResult{done: true}
		}
		return SideQueryUpdateMsg{owner: owner, generation: generation, result: result}
	}
}

func (d *sideQueryDialog) tick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return sideQueryTickMsg{owner: d} })
}

func (d *sideQueryDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if d.closed {
		return d, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		d.SetSize(msg.Width, msg.Height)
		return d, nil
	case SideQueryUpdateMsg:
		if msg.owner != d || msg.generation != d.generation || !d.running {
			return d, nil
		}
		d.answer += msg.result.text
		if msg.result.done {
			d.running, d.err = false, msg.result.err
			return d, nil
		}
		cmd := d.wait()
		return d, cmd
	case sideQueryTickMsg:
		if msg.owner == d && d.running {
			d.frame++
			cmd := d.tick()
			return d, cmd
		}
		return d, nil
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.keyMap.Close):
			d.Close()
			return d, core.CmdHandler(CloseDialogMsg{})
		case key.Matches(msg, d.keyMap.Ask):
			if d.question == "" {
				d.question = strings.TrimSpace(d.input.Value())
				if d.question != "" {
					cmd := d.start()
					return d, cmd
				}
			}
		case key.Matches(msg, d.keyMap.Retry):
			if d.err != nil && !d.running {
				cmd := d.start()
				return d, cmd
			}
		case key.Matches(msg, d.keyMap.Copy):
			if d.question != "" && !d.running && d.answer != "" {
				_ = clipboard.WriteAll(d.answer)
				return d, notification.SuccessCmd("Answer copied to clipboard.")
			}
		}
	}
	if d.question == "" {
		var cmd tea.Cmd
		d.input, cmd = d.input.Update(msg)
		return d, cmd
	}
	_, cmd := d.scrollview.Update(msg)
	return d, cmd
}

func (d *sideQueryDialog) dimensions() (width, contentWidth, maxHeight int) {
	width = max(7, d.Width()*70/100)
	if d.Width() < 70 {
		width = max(7, d.Width()-2)
	}
	contentWidth = max(1, width-styles.DialogStyle.GetHorizontalFrameSize())
	maxHeight = max(5, d.Height()*80/100)
	return
}

func (d *sideQueryDialog) Position() (row, col int) {
	width, _, _ := d.dimensions()
	return CenterPosition(d.Width(), d.Height(), width, lipgloss.Height(d.render()))
}

func (d *sideQueryDialog) activeScrollview() *scrollview.Model { return d.scrollview }

func (d *sideQueryDialog) View() string {
	rendered := d.render()
	width, _, _ := d.dimensions()
	row, col := CenterPosition(d.Width(), d.Height(), width, lipgloss.Height(rendered))
	// Title, separator, metadata, question and spacers precede the viewport.
	_, contentWidth, _ := d.dimensions()
	header := d.header(contentWidth)
	d.scrollview.SetPosition(col+styles.DialogStyle.GetBorderLeftSize()+styles.DialogStyle.GetPaddingLeft(),
		row+styles.DialogStyle.GetBorderTopSize()+styles.DialogStyle.GetPaddingTop()+lipgloss.Height(header)+1)
	return rendered
}

func (d *sideQueryDialog) header(width int) string {
	parts := []string{
		styles.DialogTitleStyle.Width(width).Render("By the way"), RenderSeparator(width),
		styles.MutedStyle.Width(width).Render("No tools · Not saved to chat"), "",
	}
	if d.question == "" {
		d.input.SetWidth(max(1, width-2))
		parts = append(parts, d.input.View())
	} else {
		question := ansi.Truncate(strings.ReplaceAll(ansi.Strip(d.question), "\n", " "), width, "…")
		parts = append(parts, styles.DialogQuestionStyle.Width(width).Render(question), RenderSeparator(width))
	}
	return strings.Join(parts, "\n")
}

func (d *sideQueryDialog) render() string {
	width, contentWidth, maxHeight := d.dimensions()
	header := d.header(contentWidth)
	help := []string{"Enter", "ask", "Esc", "close"}
	status := ""
	if d.status != nil {
		status = "Main agent: " + d.status()
	}
	if d.question != "" {
		help = []string{"↑↓", "scroll", "c", "copy", "Esc", "close"}
	}
	if d.running {
		help = []string{"↑↓", "scroll", "Esc", "cancel & close"}
	}
	if d.err != nil {
		help = []string{"r", "retry", "c", "copy", "Esc", "close"}
	}
	footer := styles.MutedStyle.Width(contentWidth).Render(status) + "\n" + RenderHelpKeys(contentWidth, help...)
	parts := []string{header}
	if d.question != "" {
		body := d.answer
		if d.running {
			frames := []string{"⠋", "⠙", "⠹", "⠸"}
			body += "\n\n" + frames[d.frame%len(frames)] + " Answering…"
		}
		if d.err != nil {
			body += "\n\nIncomplete: " + d.err.Error()
		}
		renderer := markdown.NewRendererWithoutCopyIcon(max(1, contentWidth-d.scrollview.ReservedCols()))
		rendered, err := renderer.Render(body)
		if err != nil {
			rendered = ansi.Hardwrap(ansi.Strip(body), max(1, contentWidth-1), true)
		}
		lines := strings.Split(strings.TrimRight(rendered, "\n"), "\n")
		height := max(1, min(len(lines), maxHeight-styles.DialogStyle.GetVerticalFrameSize()-lipgloss.Height(header)-lipgloss.Height(footer)-2))
		atBottom := d.scrollview.ScrollOffset() >= max(0, len(lines)-height)
		d.scrollview.SetSize(contentWidth, height)
		d.scrollview.SetContent(lines, len(lines))
		if atBottom {
			d.scrollview.ScrollToBottom()
		}
		parts = append(parts, d.scrollview.View())
	}
	parts = append(parts, footer)
	return styles.DialogStyle.Width(width).Render(strings.Join(parts, "\n\n"))
}
