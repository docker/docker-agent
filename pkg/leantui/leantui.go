package leantui

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/mattn/go-isatty"
	"github.com/muesli/cancelreader"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/gitbranch"
	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/programstatus"
	"github.com/docker/docker-agent/pkg/sound"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/editor/completions"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/streamcontent"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// Config wires the lean TUI to a prepared App and the initial run parameters.
type Config struct {
	App        *app.App
	WorkingDir string
	Cleanup    func()
	History    *history.History

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

	if cfg.History == nil {
		cfg.History, err = history.New("")
		if err != nil {
			slog.WarnContext(ctx, "Failed to initialize command history", "error", err)
		}
	}

	m := newModel(term, cfg)
	if isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd()) {
		m.programStatus = &programstatus.Reporter{}
		defer m.programStatus.Finish(os.Stdout)
		m.programStatusProbe = true
		_, _ = term.Writer().WriteString("\x1b]7501;?\x1b\\")
		_ = term.Writer().Flush()
	}
	branchWatcher, err := gitbranch.Watch(loopCtx, cfg.WorkingDir)
	if err != nil {
		return err
	}
	m.status.Branch = branchWatcher.Current()
	m.commitWelcome()
	m.loadInitialSessionTranscript()
	m.refreshCommands(loopCtx)

	keys := make(chan ui.Key, 64)
	events := make(chan any, 256)
	done := make(chan struct{})

	fileCompletion := completions.NewFileCompletionAt(loopCtx, cfg.WorkingDir)
	if loader, ok := fileCompletion.(completions.AsyncLoader); ok {
		go func() {
			items := <-loader.LoadItemsAsync(loopCtx)
			select {
			case events <- fileCompletionsLoaded(items):
			case <-done:
			}
		}()
	}

	keysDone := make(chan struct{})
	go func() {
		defer close(keysDone)
		readKeys(term.Reader(), keys, done)
	}()
	defer func() {
		close(done)
		<-keysDone
	}()
	subscriptionReady := make(chan struct{})
	go func() {
		m.app.SubscribeReliable(loopCtx, func(msg tea.Msg) {
			select {
			case events <- msg:
			case <-done:
			}
		}, app.WithSubscriptionReady(func() { close(subscriptionReady) }), app.WithGenerationEventMapper(func(msg tea.Msg, generation uint64) tea.Msg {
			return leanEvent{generation: m.eventGeneration.Load(), inner: msg, valid: func() bool { return m.app.IsEventGeneration(generation) }}
		}))
	}()
	<-subscriptionReady
	m.app.Start(loopCtx)

	if cfg.FirstMessage != nil || cfg.FirstMessageAttachment != "" {
		first := ""
		if cfg.FirstMessage != nil {
			first = *cfg.FirstMessage
		}
		m.sendFirstMessage(loopCtx, first, cfg.FirstMessageAttachment)
	}
	for _, msg := range cfg.QueuedMessages {
		if command, ok := strings.CutPrefix(strings.TrimSpace(msg), "!"); ok {
			m.runBangCommand(loopCtx, command)
			continue
		}
		m.enqueueFollowUp(msg, msg)
	}

	animationTicker := time.NewTicker(100 * time.Millisecond)
	defer animationTicker.Stop()
	resizeTicker := time.NewTicker(250 * time.Millisecond)
	defer resizeTicker.Stop()
	branchChanges := branchWatcher.Changes()

	probeTimer := time.NewTimer(200 * time.Millisecond)
	defer probeTimer.Stop()
	m.publishProgramStatus()
	m.render()
	for !m.quitting {
		select {
		case <-loopCtx.Done():
			m.quitting = true
		case <-probeTimer.C:
			m.programStatusProbe = false
			m.programStatusUnsupported = m.programStatus != nil && !m.programStatusSupported
		case k := <-keys:
			if k.Typ == ui.KeyProgramStatus {
				if m.programStatusProbe {
					m.programStatusProbe = false
					m.programStatusSupported = true
					m.publishProgramStatus()
				}
				continue
			}
			m.statusSession.Acknowledge()
			m.handleKey(loopCtx, k)
			m.publishProgramStatus()
			m.render()
		case ev := <-events:
			m.handleEvent(loopCtx, ev)
			m.publishProgramStatus()
			m.render()
		case <-resizeTicker.C:
			m.resize(term.Size())
		case <-animationTicker.C:
			if m.busy || m.screen.Transcript.ToolCount() > 0 {
				m.spinnerFrame++
				m.render()
			}
		case branch, ok := <-branchChanges:
			if !ok {
				branchChanges = nil
				continue
			}
			m.status.Branch = branch
			m.render()
		}
	}

	m.renderFinal()
	if cfg.Cleanup != nil {
		cfg.Cleanup()
	}
	return nil
}

type fileCompletionsLoaded []completion.Item

func readKeys(r io.Reader, keys chan<- ui.Key, done <-chan struct{}) {
	type readResult struct {
		data []byte
		err  error
	}
	reads := make(chan readResult)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			buf := make([]byte, 8192)
			n, err := r.Read(buf)
			select {
			case reads <- readResult{buf[:n], err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		select {
		case <-readerDone:
			return
		default:
		}
		// Some terminals cannot interrupt Read; do not hang shutdown in that case.
		if reader, ok := r.(cancelreader.CancelReader); ok && reader.Cancel() {
			<-readerDone
		}
	}()
	p := &ui.InputParser{}
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	var timeout <-chan time.Time
	send := func(events []ui.Key) bool {
		for _, key := range events {
			select {
			case keys <- key:
			case <-done:
				return false
			}
		}
		return true
	}
	for {
		select {
		case result := <-reads:
			if !send(p.Feed(result.data)) {
				return
			}
			if result.err != nil {
				send(p.Expire())
				return
			}
			if p.Waiting() {
				timer.Reset(30 * time.Millisecond)
				timeout = timer.C
			} else {
				timer.Stop()
				timeout = nil
			}
		case <-timeout:
			if !send(p.Expire()) {
				return
			}
			timeout = nil
		case <-done:
			return
		}
	}
}

type leanEvent struct {
	generation uint64
	inner      tea.Msg
	valid      func() bool
}

type model struct {
	programStatus            *programstatus.Reporter
	programStatusProbe       bool
	programStatusSupported   bool
	programStatusUnsupported bool
	statusSession            programstatus.Session
	confirmationEvent        tea.Msg
	eventGeneration          atomic.Uint64
	app                      *app.App
	term                     *ui.Terminal
	r                        *ui.Renderer

	width  int
	height int

	screen *ui.Screen

	status       ui.StatusModel
	sessionState *service.SessionState
	usage        *ui.UsageTracker

	contentIdentity     streamcontent.Tracker
	streamDepth         int
	streamStartTime     time.Time
	playSound           func(context.Context, sound.Event)
	busy                bool
	spinnerFrame        int
	runCancel           context.CancelFunc
	cancelMarkerPending bool
	queue               []ui.PendingUserMessage
	pendingUsers        []ui.PendingUserMessage
	ignoredUsers        []string

	quitting         bool
	appName          string
	banner           []string
	disabledCommands map[string]bool
	renderImages     bool
	imageSupport     bool
	sendMode         messages.SendMode
	settings         *leanSettings
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

	branch := gitbranch.Current(cfg.WorkingDir)
	screen := ui.NewScreen(cfg.WorkingDir, branch, "", cfg.History)
	screen.Transcript.SetFullFilePreview(userconfig.Get().LeanFullFilePreview)

	return &model{
		app:              cfg.App,
		playSound:        sound.Play,
		term:             term,
		r:                ui.NewRenderer(term.Writer(), w, h),
		width:            w,
		height:           h,
		screen:           screen,
		status:           ui.StatusModel{WorkingDir: cfg.WorkingDir, Branch: branch},
		sessionState:     sessionState,
		usage:            ui.NewUsageTracker(),
		appName:          appName,
		banner:           cfg.Banner,
		disabledCommands: disabled,
		renderImages:     renderImages,
		imageSupport:     renderImages,
		sendMode:         messages.ParseSendMode(userconfig.Get().GetBusySendMode()),
		hideBanner:       cfg.ShowBanner != nil && !*cfg.ShowBanner,
	}
}

func (m *model) resize(width, height int) {
	if width == m.width && height == m.height {
		return
	}
	m.width, m.height = width, height
	m.r.SetSize(width, height)
	m.render()
}

// render assembles the full frame and reconciles it with the terminal.
func (m *model) render() {
	lines, cursorLine, cursorCol := m.buildLines()
	m.r.Frame(lines, cursorLine, cursorCol, m.screen.Transcript.LiveToolRows())
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
