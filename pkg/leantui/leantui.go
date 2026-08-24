package leantui

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/gitbranch"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// Config wires the lean TUI to a prepared App and the initial run parameters.
type Config struct {
	App        *app.App
	WorkingDir string
	Cleanup    func()

	FirstMessage           *string
	FirstMessageAttachment string
	QueuedMessages         []string

	AppName          string
	DisabledCommands []string
	RenderImages     *bool
	// ShowBanner displays the ASCII-art welcome banner. Defaults to true
	// when nil.
	ShowBanner *bool

	// Banner overrides the ASCII-art welcome banner. When nil the built-in
	// bannerLines ("docker agent") is used; embedders set it to brand the lean
	// TUI with their own art (each line ideally within 56 columns).
	Banner []string
}

// Run drives the lean TUI until the user exits. It owns the terminal (raw
// mode, no alternate screen) for its lifetime and restores it on return.
func Run(ctx context.Context, cfg Config) error {
	term, err := ui.NewTerminal(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	defer term.Restore()

	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()

	m := newModel(term, cfg)
	m.commitWelcome()
	m.refreshCommands(loopCtx)

	keys := make(chan ui.Key, 64)
	events := make(chan any, 256)
	resizes := make(chan [2]int, 4)
	done := make(chan struct{})
	defer close(done)

	go readKeys(term.Reader(), keys, done)
	go func() {
		m.app.SubscribeWith(loopCtx, func(msg tea.Msg) {
			select {
			case events <- msg:
			case <-done:
			}
		})
	}()
	go func() {
		for {
			w, h, ok := term.Resized()
			if !ok {
				return
			}
			select {
			case resizes <- [2]int{w, h}:
			case <-done:
				return
			}
		}
	}()

	if cfg.FirstMessage != nil || cfg.FirstMessageAttachment != "" {
		first := ""
		if cfg.FirstMessage != nil {
			first = *cfg.FirstMessage
		}
		m.sendFirstMessage(loopCtx, first, cfg.FirstMessageAttachment)
	}
	for _, msg := range cfg.QueuedMessages {
		m.enqueueFollowUp(msg, msg)
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	m.render()
	for !m.quitting {
		select {
		case <-loopCtx.Done():
			m.quitting = true
		case k := <-keys:
			m.handleKey(loopCtx, k)
			m.render()
		case ev := <-events:
			m.handleEvent(loopCtx, ev)
			m.render()
		case sz := <-resizes:
			m.width, m.height = sz[0], sz[1]
			m.r.SetSize(sz[0], sz[1])
			m.render()
		case <-ticker.C:
			if m.busy {
				m.spinnerFrame++
				m.render()
			}
		}
	}

	m.renderFinal()
	if cfg.Cleanup != nil {
		cfg.Cleanup()
	}
	return nil
}

func readKeys(r io.Reader, keys chan<- ui.Key, done <-chan struct{}) {
	p := &ui.InputParser{}
	buf := make([]byte, 8192)
	for {
		n, err := r.Read(buf)
		for _, k := range p.Feed(buf[:n]) {
			select {
			case keys <- k:
			case <-done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

type model struct {
	app  *app.App
	term *ui.Terminal
	r    *ui.Renderer

	width  int
	height int

	screen *ui.Screen

	status       ui.StatusModel
	sessionState *service.SessionState
	usage        *ui.UsageTracker

	busy         bool
	spinnerFrame int
	runCancel    context.CancelFunc
	queue        []ui.PendingUserMessage
	pendingUsers []ui.PendingUserMessage
	ignoredUsers []string

	quitting         bool
	appName          string
	banner           []string
	disabledCommands map[string]bool
	renderImages     bool
	// hideBanner drops the ASCII-art welcome banner; the zero value keeps it.
	hideBanner bool
}

func newModel(term *ui.Terminal, cfg Config) *model {
	w, h := term.Size()
	appName := cfg.AppName
	if appName == "" {
		appName = "docker agent"
	}
	disabled := make(map[string]bool, len(cfg.DisabledCommands))
	for _, c := range cfg.DisabledCommands {
		disabled[strings.TrimPrefix(c, "/")] = true
	}

	sessionState := service.NewSessionState(nil)
	if cfg.App != nil {
		sessionState = service.NewSessionState(cfg.App.Session())
	}

	renderImages := cfg.RenderImages == nil || *cfg.RenderImages

	return &model{
		app:              cfg.App,
		term:             term,
		r:                ui.NewRenderer(term.Writer(), w, h),
		width:            w,
		height:           h,
		screen:           ui.NewScreen(cfg.WorkingDir, gitbranch.Current(cfg.WorkingDir), "Type a message, / for commands"),
		status:           ui.StatusModel{WorkingDir: cfg.WorkingDir, Branch: gitbranch.Current(cfg.WorkingDir)},
		sessionState:     sessionState,
		usage:            ui.NewUsageTracker(),
		appName:          appName,
		banner:           cfg.Banner,
		disabledCommands: disabled,
		renderImages:     renderImages,
		hideBanner:       cfg.ShowBanner != nil && !*cfg.ShowBanner,
	}
}

// render assembles the full frame and reconciles it with the terminal.
func (m *model) render() {
	lines, cursorLine, cursorCol := m.buildLines()
	m.r.Frame(lines, cursorLine, cursorCol)
}

// renderFinal repaints the current state, then erases the input box and footer
// so only the conversation remains once the program exits.
func (m *model) renderFinal() {
	m.screen.Transcript.FlushPending()
	m.render()
	m.r.EraseBelow(len(m.screen.Transcript.Lines(m.width, m.spinnerFrame, m.busy, m.sessionState, m.pendingUsers)))
}

func (m *model) commitWelcome() {
	banner := m.banner
	if len(banner) == 0 {
		banner = bannerLines
	}
	if m.hideBanner {
		banner = nil
	}
	m.screen.Transcript.AddBlock(func(int) []string {
		lines := make([]string, 0, bannerTopPadding+len(banner)+2)
		for range bannerTopPadding {
			lines = append(lines, "")
		}

		leftPad := strings.Repeat(" ", bannerLeftPadding)
		for _, l := range banner {
			lines = append(lines, ui.StAccent().Render(leftPad+l))
		}
		lines = append(lines,
			"",
			ui.StMuted().Render(leftPad+"Type a message, press / for commands, Ctrl+C to quit."),
		)
		return lines
	})
}
