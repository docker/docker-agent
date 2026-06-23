package leantui

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
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
}

// Run drives the lean TUI until the user exits. It owns the terminal (raw
// mode, no alternate screen) for its lifetime and restores it on return.
func Run(ctx context.Context, cfg Config) error {
	term, err := newTerminal(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	defer term.restore()

	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()

	m := newModel(term, cfg)
	m.commitWelcome()
	m.refreshCommands(loopCtx)

	keys := make(chan key, 64)
	events := make(chan any, 256)
	resizes := make(chan [2]int, 4)
	done := make(chan struct{})
	defer close(done)

	go readKeys(term.reader, keys, done)
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
			w, h, ok := term.resized()
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
	m.queue = append(m.queue, cfg.QueuedMessages...)

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
			m.r.setSize(sz[0], sz[1])
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

func readKeys(r io.Reader, keys chan<- key, done <-chan struct{}) {
	p := &inputParser{}
	buf := make([]byte, 8192)
	for {
		n, err := r.Read(buf)
		for _, k := range p.feed(buf[:n]) {
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

type blockKind int

const (
	blockReasoning blockKind = iota
	blockAssistant
)

type pendingBlock struct {
	kind blockKind
	text strings.Builder
}

type model struct {
	app  *app.App
	term *terminal
	r    *renderer

	width  int
	height int

	editor *editor
	ac     *autocomplete

	status statusData

	blocks       []*block
	busy         bool
	spinnerFrame int
	pending      *pendingBlock
	tools        map[string]*toolView
	toolOrder    []string
	toolStart    map[string]time.Time

	runCancel context.CancelFunc
	queue     []string

	confirm *confirmState

	quitting         bool
	appName          string
	disabledCommands map[string]bool
}

func newModel(term *terminal, cfg Config) *model {
	w, h := term.size()
	appName := cfg.AppName
	if appName == "" {
		appName = "docker agent"
	}
	disabled := make(map[string]bool, len(cfg.DisabledCommands))
	for _, c := range cfg.DisabledCommands {
		disabled[strings.TrimPrefix(c, "/")] = true
	}

	return &model{
		app:              cfg.App,
		term:             term,
		r:                newRenderer(term.writer, w, h),
		width:            w,
		height:           h,
		editor:           newEditor("Type a message, / for commands"),
		ac:               newAutocomplete(),
		tools:            make(map[string]*toolView),
		toolStart:        make(map[string]time.Time),
		status:           statusData{workingDir: cfg.WorkingDir, branch: gitBranch(cfg.WorkingDir)},
		appName:          appName,
		disabledCommands: disabled,
	}
}

// render assembles the full frame and reconciles it with the terminal.
func (m *model) render() {
	lines, cursorLine, cursorCol := m.buildLines()
	m.r.frame(lines, cursorLine, cursorCol)
}

// renderFinal repaints the current state, then erases the input box and footer
// so only the conversation remains once the program exits.
func (m *model) renderFinal() {
	m.flushPending()
	m.render()
	m.r.eraseBelow(len(m.conversationLines(m.width)))
}

// addBlock appends a finalized, lazily-rendered block to the conversation.
func (m *model) addBlock(render func(width int) []string) {
	m.blocks = append(m.blocks, &block{render: render})
}

func (m *model) commitWelcome() {
	m.addBlock(func(int) []string {
		lines := make([]string, 0, len(bannerLines)+2)
		for _, l := range bannerLines {
			lines = append(lines, stAccent().Render(l))
		}
		lines = append(lines,
			"",
			stMuted().Render("Type a message, press / for commands, Ctrl+C to quit."),
		)
		return lines
	})
}
