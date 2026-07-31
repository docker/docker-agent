package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app/export"
	"github.com/docker/docker-agent/pkg/app/transcript"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/shellpath"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type App struct {
	ctx func() context.Context

	runtime                runtime.Runtime
	session                *session.Session
	firstMessage           *string
	firstMessageAttach     string
	queuedMessages         []string
	events                 chan tea.Msg
	throttleDuration       time.Duration
	cancel                 context.CancelFunc
	currentAgentModel      string                      // Tracks the current agent's model ID from AgentInfoEvent
	exitAfterFirstResponse bool                        // Exit TUI after first assistant response completes
	readOnly               bool                        // When true, no new messages can be sent to the LLM
	titleGenerating        atomic.Bool                 // True when title generation is in progress
	titleGen               *sessiontitle.Generator     // Title generator for local runtime (nil for remote)
	snapshotController     builtins.SnapshotController // Drives /undo, /snapshots, /reset; nil for runtimes that don't capture snapshots
	streamGuard            sync.Locker                 // Held for the duration of every direct RunStream call; nil when not attached to a SessionManager (see WithStreamGuard)

	startOnce  sync.Once
	subsMu     sync.Mutex
	subs       []chan tea.Msg
	fanoutOnce sync.Once
}

// Opt is an option for creating a new App.
type Opt func(*App)

// WithFirstMessage sets the first message to send.
func WithFirstMessage(msg string) Opt {
	return func(a *App) {
		a.firstMessage = &msg
	}
}

// WithFirstMessageAttachment sets the attachment path for the first message.
func WithFirstMessageAttachment(path string) Opt {
	return func(a *App) {
		a.firstMessageAttach = path
	}
}

// WithExitAfterFirstResponse configures the app to exit after the first assistant response.
func WithExitAfterFirstResponse() Opt {
	return func(a *App) {
		a.exitAfterFirstResponse = true
	}
}

// WithQueuedMessages sets messages to be queued after the first message is sent.
// These messages will be delivered to the TUI as SendMsg events, which the
// chat page will queue and process sequentially after each agent response.
func WithQueuedMessages(msgs []string) Opt {
	return func(a *App) {
		a.queuedMessages = msgs
	}
}

// WithTitleGenerator sets the title generator for local title generation.
// If not set, title generation will be handled by the runtime (for remote) or skipped.
func WithTitleGenerator(gen *sessiontitle.Generator) Opt {
	return func(a *App) {
		a.titleGen = gen
	}
}

// WithReadOnly marks the session as read-only: the conversation history
// is displayed but no new messages can be sent to the LLM.
func WithReadOnly() Opt {
	return func(a *App) {
		a.readOnly = true
	}
}

// WithSnapshotController plumbs in the [builtins.SnapshotController]
// the App uses to drive /undo, /snapshots, /reset. Pass the same
// controller to the runtime via runtime.WithAutoInjector so the
// instance that captures the checkpoints is the one the TUI commands
// drive. Pass nil (or omit the option) for runtimes that don't capture
// snapshots; the App then reports SnapshotsEnabled()==false and the
// related commands silently no-op.
func WithSnapshotController(c builtins.SnapshotController) Opt {
	return func(a *App) {
		a.snapshotController = c
	}
}

// WithStreamGuard makes the App hold l for the duration of every direct
// RunStream call (Run, Retry, RunWithMessage). When the App is attached to a
// SessionManager (the --listen control plane or the local recall
// coordinator), l is the SAME lock RunSession and AddMessage/UpdateMessage
// already use (via TryLock) to detect a session's active stream, so a
// concurrent REST mutation correctly sees the App's own stream as busy
// instead of racing it (#3590). Without this option (a bare App with no
// attached SessionManager) there is nothing to race against, so omitting it
// is safe.
func WithStreamGuard(l sync.Locker) Opt {
	return func(a *App) {
		a.streamGuard = l
	}
}

func New(ctx context.Context, rt runtime.Runtime, sess *session.Session, opts ...Opt) *App {
	app := &App{
		ctx:              func() context.Context { return context.WithoutCancel(ctx) },
		runtime:          rt,
		session:          sess,
		events:           make(chan tea.Msg, 128),
		throttleDuration: 50 * time.Millisecond, // Throttle rapid events
	}

	for _, opt := range opts {
		opt(app)
	}

	return app
}

// Start begins App-owned background event producers. Construction stays cheap
// and side-effect free; embedders call Start when the App enters a managed
// lifecycle.
func (a *App) Start(ctx context.Context) {
	a.startOnce.Do(func() {
		// Emit startup info (agent, team, tools) through the events channel.
		// This runs in the background so the TUI can start immediately while
		// slow operations (like MCP tool loading) complete asynchronously.
		go func() {
			startupEvents := make(chan runtime.Event, 10)
			go func() {
				defer close(startupEvents)
				a.runtime.EmitStartupInfo(ctx, a.session, runtime.NewChannelSink(startupEvents))
			}()
			for event := range startupEvents {
				select {
				case a.events <- event:
				case <-ctx.Done():
					return
				}
			}
		}()

		// Subscribe to tool list changes so the sidebar updates immediately
		// when an MCP server adds or removes tools (outside of a RunStream).
		a.runtime.OnToolsChanged(func(event runtime.Event) {
			select {
			case a.events <- event:
			case <-ctx.Done():
			}
		})

		// Forward events surfaced from detached background work (token usage
		// from background agent tasks) so the sidebar and agent inspector can
		// account for background agents' context usage.
		a.runtime.OnBackgroundEvent(func(event runtime.Event) {
			select {
			case a.events <- event:
			case <-ctx.Done():
			}
		})

		// Forward elicitation requests raised anywhere in the runtime —
		// including background-job (run_background_agent) sub-sessions whose
		// RunStream has no live UI reading its own events channel — so they
		// always reach the TUI as a dialog. For runtimes that mirror sink
		// deliveries onto their RunStream channel too (LocalRuntime: the
		// swap-based bridge best-effort-sends the same event on the very
		// RunStream channel Run/Retry/RunWithMessage read from below), this
		// sink is the single, exactly-once delivery point and those loops skip
		// the mirrored copy (see mustSkipMirroredElicitation). Runtimes that
		// don't mirror it (RemoteRuntime, whose OnElicitationRequest below is
		// a no-op) deliver elicitations only through that RunStream copy,
		// which those loops forward unfiltered (#3584 review).
		a.runtime.OnElicitationRequest(func(event runtime.Event) {
			a.sendEvent(ctx, event)
		})
	})
}

func (a *App) SendFirstMessage() tea.Cmd {
	if a.firstMessage == nil {
		return nil
	}

	cmds := []tea.Cmd{
		func() tea.Msg {
			// Use the shared PrepareUserMessage function for consistent attachment handling
			userMsg, attachedPath, err := cli.PrepareUserMessage(a.ctx(), a.runtime, *a.firstMessage, a.firstMessageAttach)
			if err != nil {
				slog.Error("Failed to prepare first message", "error", err)
				return nil
			}
			if userMsg == nil {
				// Agent-only command with no content - agent switched but no message to send
				return nil
			}
			// Inherit the attachment in any sub-session created by this turn.
			a.session.AddAttachedFile(attachedPath)

			// If the message has multi-content (attachments), we need to handle it specially
			if len(userMsg.Message.MultiContent) > 0 {
				return messages.SendAttachmentMsg{
					Content: userMsg,
				}
			}

			return messages.SendMsg{
				Content: userMsg.Message.Content,
			}
		},
	}

	// Queue additional messages to be sent after the first one.
	// The TUI's message queue will hold them until the agent finishes
	// processing the previous message.
	for _, msg := range a.queuedMessages {
		cmds = append(cmds, func() tea.Msg {
			return messages.SendMsg{
				Content: msg,
			}
		})
	}

	return tea.Sequence(cmds...)
}

// CurrentAgentTools returns the tools available to the current agent.
func (a *App) CurrentAgentTools(ctx context.Context) ([]tools.Tool, error) {
	return a.runtime.CurrentAgentTools(ctx)
}

// agentConfigProvider is an optional runtime capability: exposing an agent's
// static configuration (toolsets, sub-agents, handoffs, fallbacks) by name.
// Only the local runtime (which holds the team) implements it; remote runtimes
// don't, so the agent-details config sections are simply omitted for them.
type agentConfigProvider interface {
	AgentConfigInfo(ctx context.Context, agentName string) runtime.AgentConfigInfo
}

// AgentConfigInfo returns the named agent's static configuration for the
// read-only agent-details dialog, or the zero value when it can't be resolved
// (remote runtime or unknown agent). It reads resolved config only and starts
// no toolsets.
func (a *App) AgentConfigInfo(ctx context.Context, agentName string) runtime.AgentConfigInfo {
	cp, ok := a.runtime.(agentConfigProvider)
	if !ok {
		return runtime.AgentConfigInfo{}
	}
	return cp.AgentConfigInfo(ctx, agentName)
}

// CurrentAgentToolsetStatuses returns lifecycle status for each toolset of
// the active agent.
func (a *App) CurrentAgentToolsetStatuses() []tools.ToolsetStatus {
	return a.runtime.CurrentAgentToolsetStatuses()
}

// RestartToolset triggers a supervisor-driven restart of the named toolset.
func (a *App) RestartToolset(ctx context.Context, name string) error {
	return a.runtime.RestartToolset(ctx, name)
}

// agentThinkingLevelsProvider is an optional runtime capability: resolving
// the thinking-effort levels the current agent's active model supports.
// Only the local runtime (which holds the agent and its model config)
// implements it; remote runtimes don't, so /effort argument completion
// simply offers no candidates for them.
type agentThinkingLevelsProvider interface {
	CurrentAgentThinkingLevels(ctx context.Context) []effort.Level
}

// CurrentAgentThinkingLevels returns the thinking-effort levels supported by
// the current agent's active model, or nil when the runtime cannot resolve
// this (remote runtime, unresolvable agent/model, or a model that does not
// support thinking at all). Backs the /effort argument completer (#3731).
func (a *App) CurrentAgentThinkingLevels(ctx context.Context) []effort.Level {
	p, ok := a.runtime.(agentThinkingLevelsProvider)
	if !ok {
		return nil
	}
	return p.CurrentAgentThinkingLevels(ctx)
}

// CurrentAgentCommands returns the commands for the active agent
func (a *App) CurrentAgentCommands(ctx context.Context) types.Commands {
	return a.runtime.CurrentAgentInfo(ctx).Commands
}

// CurrentAgentSkills returns the available skills if skills are enabled for the current agent.
func (a *App) CurrentAgentSkills() []skills.Skill {
	st := a.runtime.CurrentAgentSkillsToolset()
	if st == nil {
		return nil
	}
	return st.Skills()
}

// ResolveSkillCommand checks if the input matches a skill slash command (e.g. /skill-name args).
// If matched, it reads the skill content and returns the resolved prompt. Otherwise returns "".
//
// Fork-mode skills are NOT resolved here; chat dispatches them via
// SkillCommandFork + RunSkillFork to keep the parent transcript clean.
func (a *App) ResolveSkillCommand(ctx context.Context, input string) (string, error) {
	if !strings.HasPrefix(input, "/") {
		return "", nil
	}

	st := a.runtime.CurrentAgentSkillsToolset()
	if st == nil {
		return "", nil
	}

	cmd, arg, _ := strings.Cut(input[1:], " ")
	arg = strings.TrimSpace(arg)

	for _, skill := range st.Skills() {
		if skill.Name != cmd {
			continue
		}

		if skill.IsFork() {
			// Fall through to ResolveCommand for non-chat callers; the
			// chat layer already routed fork-mode skills via
			// SkillCommandFork before reaching this point.
			return "", nil
		}

		// NopRuntime refuses the commands the body embeds instead of running
		// them: input resolution is synchronous inside the UI loop, so there is
		// no way to raise an approval prompt here. Each refusal is inlined in
		// the content the agent receives, and the agent can still run the
		// command itself through its own (gated) shell tool.
		content, err := st.ReadSkillContent(ctx, skill.Name, tools.NopRuntime{})
		if err != nil {
			return "", fmt.Errorf("reading skill %q: %w", skill.Name, err)
		}

		if arg != "" {
			return fmt.Sprintf("Use the following skill.\n\nUser's request: %s\n\n<skill name=%q>\n%s\n</skill>", arg, skill.Name, content), nil
		}
		return fmt.Sprintf("Use the following skill.\n\n<skill name=%q>\n%s\n</skill>", skill.Name, content), nil
	}

	return "", nil
}

// SkillCommandFork returns (skillName, task, true) when input is a slash
// command for a `context: fork` skill, otherwise (_, _, false). Chat layers
// must call this before ResolveInput and route to RunSkillFork on a hit.
func (a *App) SkillCommandFork(_ context.Context, input string) (skillName, task string, ok bool) {
	if !strings.HasPrefix(input, "/") {
		return "", "", false
	}

	st := a.runtime.CurrentAgentSkillsToolset()
	if st == nil {
		return "", "", false
	}

	cmd, arg, _ := strings.Cut(input[1:], " ")
	arg = strings.TrimSpace(arg)

	for _, skill := range st.Skills() {
		if skill.Name != cmd {
			continue
		}
		if !skill.IsFork() {
			return "", "", false
		}
		return skill.Name, arg, true
	}

	return "", "", false
}

// RunSkillFork dispatches a fork-mode skill in an isolated sub-session of
// the current parent. The parent gains a SubSession item once the runtime
// opens the child; the sub-session's first user message is the expanded
// SKILL.md body. Companion of SkillCommandFork.
func (a *App) RunSkillFork(ctx context.Context, cancel context.CancelFunc, skillName, task string, _ []messages.Attachment) {
	a.cancel = cancel

	// Mirrors App.Run's drain loop: forward events to the App bus and
	// always let StreamStoppedEvent through, even after ctx cancellation,
	// so the supervisor marks the session idle.
	go func() {
		events := make(chan runtime.Event, defaultRuntimeEventBuffer)
		var failed atomic.Bool
		go func() {
			defer close(events)
			result, err := a.runtime.RunSkillFork(ctx, a.session, skillstool.RunSkillArgs{
				Name: skillName,
				Task: task,
			}, runtime.NewChannelSink(events))
			switch {
			case errors.Is(err, runtime.ErrUnsupported):
				slog.WarnContext(ctx, "Runtime does not support fork-mode skills; skill not executed", "skill", skillName)
				failed.Store(true)
				a.sendEvent(ctx, runtime.Error(fmt.Sprintf("Skill %q cannot run: this runtime does not support fork-mode skills.", skillName)))
			case err != nil:
				slog.ErrorContext(ctx, "Failed to run fork-mode skill", "skill", skillName, "error", err)
				failed.Store(true)
				a.sendEvent(ctx, runtime.Error(fmt.Sprintf("Skill %q failed: %v", skillName, err)))
			case result != nil && result.IsError:
				failed.Store(true)
				a.sendEvent(ctx, runtime.Error(result.Output))
			}
		}()

		// sawStop tracks ANY StreamStoppedEvent forwarded here — unlike
		// forwardRunStreamEvents' root-only rule (#4136), every event on this
		// channel belongs to the fork's own sub-session, so a root-session
		// check would always be false here and synthesize a spurious
		// duplicate stop on every fork run, successful or not.
		var (
			sawStop       bool
			lastSessionID string
			agentName     string
		)
		for event := range events {
			if scoped, ok := event.(runtime.SessionScoped); ok {
				if id := scoped.GetSessionID(); id != "" {
					lastSessionID = id
				}
			}
			if name := event.GetAgentName(); name != "" {
				agentName = name
			}
			if _, ok := event.(*runtime.StreamStoppedEvent); ok {
				sawStop = true
			}

			if ctx.Err() != nil {
				if _, ok := event.(*runtime.StreamStoppedEvent); ok {
					// ctx is cancelled; detach cancellation but keep its trace
					// context so the stop event still reaches subscribers.
					a.sendEvent(context.WithoutCancel(ctx), event)
				}
				continue
			}
			a.sendEvent(ctx, event)
		}

		if !sawStop {
			a.synthesizeStreamStopped(ctx, cmp.Or(lastSessionID, a.session.ID), agentName, failed.Load())
		}
	}()
}

// defaultRuntimeEventBuffer matches Summarize and Runtime.RunStream;
// wide enough that a fork-skill sub-session won't block the producer.
const defaultRuntimeEventBuffer = 100

// ResolveInput resolves the user input by trying skill commands first,
// then agent commands. Returns the resolved content ready to send to the agent.
func (a *App) ResolveInput(ctx context.Context, input string) string {
	if resolved, err := a.ResolveSkillCommand(ctx, input); err != nil {
		return fmt.Sprintf("Error loading skill: %v", err)
	} else if resolved != "" {
		return resolved
	}

	return a.ResolveCommand(ctx, input)
}

// CurrentAgentModel returns the model ID for the current agent.
// Returns the tracked model from AgentInfoEvent, or falls back to session overrides.
// Returns empty string if no model information is available (fail-open scenario).
func (a *App) CurrentAgentModel(ctx context.Context) string {
	if a.currentAgentModel != "" {
		return a.currentAgentModel
	}
	// Fallback to session overrides
	if a.session != nil && a.session.AgentModelOverrides != nil {
		agentName := a.runtime.CurrentAgentName(ctx)
		if modelRef, ok := a.session.AgentModelOverrides[agentName]; ok {
			return modelRef
		}
	}
	return ""
}

// TrackCurrentAgentModel updates the tracked model ID for the current agent.
// This is called when AgentInfoEvent is received from the runtime.
func (a *App) TrackCurrentAgentModel(model string) {
	a.currentAgentModel = model
}

// CurrentMCPPrompts returns the available MCP prompts for the active agent
func (a *App) CurrentMCPPrompts(ctx context.Context) map[string]mcptools.PromptInfo {
	return a.runtime.CurrentMCPPrompts(ctx)
}

// ExecuteMCPPrompt executes an MCP prompt with provided arguments and returns the content
func (a *App) ExecuteMCPPrompt(ctx context.Context, promptName string, arguments map[string]string) (string, error) {
	return a.runtime.ExecuteMCPPrompt(ctx, promptName, arguments)
}

// ResolveCommand converts /command to its prompt text
func (a *App) ResolveCommand(ctx context.Context, userInput string) string {
	return runtime.ResolveCommand(ctx, a.runtime, userInput)
}

// LookupCommand parses userInput as a /command invocation and returns the
// matching command, the trailing arguments, and whether a match was found.
// Callers that want to act on command metadata (for example switching to a
// sub-agent declared via the `agent:` field) should call this before
// ResolveCommand to inspect the raw command.
func (a *App) LookupCommand(ctx context.Context, userInput string) (types.Command, string, bool) {
	return runtime.LookupCommand(ctx, a.runtime, userInput)
}

// EmitStartupInfo emits initial agent, team, and toolset information to the provided channel
func (a *App) EmitStartupInfo(ctx context.Context, events chan runtime.Event) {
	a.runtime.EmitStartupInfo(ctx, a.session, runtime.NewChannelSink(events))
}

// Run one agent loop
func (a *App) Run(ctx context.Context, cancel context.CancelFunc, message string, attachments []messages.Attachment) {
	a.cancel = cancel

	// If this is the first message and no title exists, start local title generation
	if a.session.TitleSnapshot() == "" && a.titleGen != nil {
		a.titleGenerating.Store(true)
		go a.generateTitle(ctx, []string{message})
	}

	go func() {
		release := a.acquireStreamGuard()
		defer release()

		if len(attachments) > 0 {
			multiContent := a.buildUserMultiContent(ctx, message, attachments)
			a.session.AddMessage(session.UserMessage(message, multiContent...))
		} else {
			a.session.AddMessage(session.UserMessage(message))
		}
		a.forwardRunStreamEvents(ctx, a.runtime.RunStream(ctx, a.session), nil)
	}()
}

// buildUserMultiContent assembles the MultiContent parts for a user message
// with attachments. It builds a single text string with the user's message
// and inlined text files — keeping everything in one text block ensures the
// model sees file content together with the message, rather than as separate
// content blocks — followed by any binary parts (images, PDFs, …).
func (a *App) buildUserMultiContent(ctx context.Context, message string, attachments []messages.Attachment) []chat.MessagePart {
	var textBuilder strings.Builder
	textBuilder.WriteString(message)

	// binaryParts holds non-text file parts (images, PDFs, etc.)
	var binaryParts []chat.MessagePart

	for _, att := range attachments {
		switch {
		case att.FilePath != "":
			// File-reference attachment: read and classify from disk.
			// Only remember the path on the session when the file actually
			// exists as a regular file — we don't want sub-agents to inherit
			// dangling references to directories or missing paths. The editor
			// resolves @-mentions to absolute paths before this point.
			if a.processFileAttachment(ctx, att, &textBuilder, &binaryParts) {
				a.session.AddAttachedFile(att.FilePath)
			}
		case att.Content != "":
			// Inline content attachment (e.g. pasted text).
			a.processInlineAttachment(att, &textBuilder)
		case len(att.Data) > 0:
			// Binary inline content.
			doc, resizeMeta, procErr := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
				Type: chat.MessagePartTypeDocument,
				Document: &chat.Document{
					Name:     att.Name,
					MimeType: att.MimeType,
					Source: chat.DocumentSource{
						InlineData: att.Data,
					},
				},
			})
			if procErr != nil {
				slog.WarnContext(ctx, "skipping inline attachment: processing failed", "name", att.Name, "error", procErr)
				a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: %s", att.Name, procErr), ""))
				continue
			}
			if resizeMeta != nil {
				if note := chat.FormatDimensionNote(resizeMeta); note != "" {
					textBuilder.WriteString("\n")
					textBuilder.WriteString(note)
				}
			}
			binaryParts = append(binaryParts, chat.MessagePart{
				Type:     chat.MessagePartTypeDocument,
				Document: &doc,
			})
		default:
			slog.DebugContext(ctx, "skipping attachment with no file path, content, or data", "name", att.Name)
		}
	}

	multiContent := []chat.MessagePart{
		{Type: chat.MessagePartTypeText, Text: textBuilder.String()},
	}
	return append(multiContent, binaryParts...)
}

// processFileAttachment reads a file from disk, classifies it, and either
// appends its text content to textBuilder or adds a binary part to binaryParts.
// Returns true when the path resolved to a real, regular file that we attempted
// to surface to the model — even if the content itself was rejected (too
// large, unsupported MIME, transient read error, etc.). The boolean is meant
// for callers that want to record the path on the session for later reuse by
// sub-agents; we don't want those references to point at directories or
// missing files, but we do want them to cover "the agent has bigger tools
// than us" cases.
func (a *App) processFileAttachment(ctx context.Context, att messages.Attachment, textBuilder *strings.Builder, binaryParts *[]chat.MessagePart) bool {
	absPath := att.FilePath

	fi, err := os.Stat(absPath)
	if err != nil {
		var reason string
		switch {
		case os.IsNotExist(err):
			reason = "file does not exist"
		case os.IsPermission(err):
			reason = "permission denied"
		default:
			reason = fmt.Sprintf("cannot access file: %v", err)
		}
		slog.WarnContext(ctx, "skipping attachment", "path", absPath, "reason", reason)
		a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: %s", att.Name, reason), ""))
		return false
	}

	if !fi.Mode().IsRegular() {
		slog.WarnContext(ctx, "skipping attachment: not a regular file", "path", absPath, "mode", fi.Mode().String())
		a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: not a regular file", att.Name), ""))
		return false
	}

	const maxAttachmentSize = 100 * 1024 * 1024 // 100MB
	if fi.Size() > maxAttachmentSize {
		slog.WarnContext(ctx, "skipping attachment: file too large", "path", absPath, "size", fi.Size(), "max", maxAttachmentSize)
		a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: file too large (max 100MB)", att.Name), ""))
		return true
	}

	mimeType := chat.DetectMimeType(absPath)

	switch {
	case chat.IsTextFile(absPath):
		if fi.Size() > chat.MaxInlineFileSize {
			slog.WarnContext(ctx, "skipping attachment: text file too large to inline", "path", absPath, "size", fi.Size(), "max", chat.MaxInlineFileSize)
			a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: text file too large to inline (max 5MB)", att.Name), ""))
			return true
		}
		content, err := chat.ReadFileForInline(absPath)
		if err != nil {
			slog.WarnContext(ctx, "skipping attachment: failed to read file", "path", absPath, "error", err)
			a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: failed to read file", att.Name), ""))
			return true
		}
		textBuilder.WriteString("\n\n")
		textBuilder.WriteString(content)

	case chat.IsSupportedMimeType(mimeType):
		// Route through ProcessAttachmentWithMetadata for normalised Document output.
		// For images this also returns resize metadata used to emit a dimension note.
		doc, resizeMeta, procErr := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
			Type: chat.MessagePartTypeFile,
			File: &chat.MessageFile{Path: absPath, MimeType: mimeType},
		})
		if procErr != nil {
			slog.WarnContext(ctx, "skipping attachment: processing failed", "path", absPath, "error", procErr)
			a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: %s", att.Name, procErr), ""))
			return true
		}
		// For images, emit a dimension note so the model can map coordinates back to the original.
		if resizeMeta != nil {
			if note := chat.FormatDimensionNote(resizeMeta); note != "" {
				textBuilder.WriteString("\n" + note)
			}
		}
		*binaryParts = append(*binaryParts, chat.MessagePart{
			Type:     chat.MessagePartTypeDocument,
			Document: &doc,
		})

	default:
		slog.WarnContext(ctx, "skipping attachment: unsupported file type", "path", absPath, "mime_type", mimeType)
		a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: unsupported file type", att.Name), ""))
	}

	return true
}

// sendEvent sends an event to the TUI, respecting context cancellation to
// avoid blocking on the channel when the consumer has stopped reading.
func (a *App) sendEvent(ctx context.Context, event tea.Msg) {
	select {
	case a.events <- event:
	case <-ctx.Done():
	}
}

// isElicitationRequestEvent reports whether event is an
// *runtime.ElicitationRequestEvent.
func isElicitationRequestEvent(event runtime.Event) bool {
	_, ok := event.(*runtime.ElicitationRequestEvent)
	return ok
}

// elicitationSinkMirror is an optional runtime capability satisfied by
// runtimes whose OnElicitationRequest sink is the single, exactly-once
// delivery point for elicitation requests even though the runtime ALSO
// best-effort-mirrors the same event onto its RunStream channel, for the
// benefit of out-of-process consumers reading RunStream directly (see
// runtime.LocalRuntime.MirrorsElicitationOnRunStream and elicitationHandler,
// #3584).
type elicitationSinkMirror interface {
	MirrorsElicitationOnRunStream()
}

// mustSkipMirroredElicitation reports whether rt implements
// elicitationSinkMirror, i.e. whether Start's OnElicitationRequest sink
// (below) already delivers every elicitation from rt exactly once, making
// any *runtime.ElicitationRequestEvent read directly off RunStream a
// duplicate that Run/Retry/RunWithMessage must skip to avoid a second
// dialog for the same request.
//
// Runtimes that do NOT implement it (RemoteRuntime, whose
// OnElicitationRequest is a documented no-op) deliver elicitations ONLY via
// RunStream, so that copy is the sole delivery and must reach a.events
// unfiltered — an earlier fix skipped this event unconditionally and
// silently dropped every remote elicitation as a result (#3584 review).
func mustSkipMirroredElicitation(rt runtime.Runtime) bool {
	_, ok := rt.(elicitationSinkMirror)
	return ok
}

// forwardRunStreamEvents drains ch and forwards each event to a.events,
// applying the bookkeeping shared by Run, Retry, and RunWithMessage. This is
// the SINGLE place that decides whether a RunStream event reaches a.events:
// centralizing it here (instead of three copies of the same loop) is what
// lets one set of tests cover all three entry points, and makes it
// impossible for one of them to silently regress independently of the
// others (#3584 review — Retry and RunWithMessage had duplicated this exact
// logic untested).
//
// filter, when non-nil, runs after the elicitation-mirroring skip below and
// may itself veto forwarding an event by returning false. Retry uses it to
// suppress the pre-StreamStarted re-emitted user message; Run and
// RunWithMessage pass nil.
func (a *App) forwardRunStreamEvents(ctx context.Context, ch <-chan runtime.Event, filter func(event runtime.Event) (forward bool)) {
	skipMirroredElicitation := mustSkipMirroredElicitation(a.runtime)

	// sawRootStop/sawRootError/agentName drive the #4136 fallback below: the
	// runtime documents channel close (not receipt of StreamStoppedEvent) as
	// the terminal signal, and may drop the event under back-pressure
	// (LocalRuntime.finalizeEventChannel). Only a root-session stop counts —
	// a sub-session (delegation/fork) stop leaves the root stream running.
	var (
		sawRootStop  bool
		sawRootError bool
		agentName    string
	)

	for event := range ch {
		isRoot := isRootSessionEvent(event, a.session.ID)
		if isRoot {
			if name := event.GetAgentName(); name != "" {
				agentName = name
			}
			switch event.(type) {
			case *runtime.StreamStoppedEvent:
				// Tracked here — before the cancellation/filter/dedupe checks
				// below — so a filter that happens to veto a genuine root stop
				// can never leave this bookkeeping thinking none arrived and
				// synthesize a spurious duplicate.
				sawRootStop = true
			case *runtime.ErrorEvent:
				sawRootError = true
			}
		}

		// If context is cancelled, continue draining but don't forward events
		// — except StreamStoppedEvent, which must always propagate so the
		// supervisor can mark the session as no longer running.
		if ctx.Err() != nil {
			if _, ok := event.(*runtime.StreamStoppedEvent); ok {
				// ctx is cancelled; detach cancellation but keep its trace
				// context so the stop event still reaches subscribers.
				a.sendEvent(context.WithoutCancel(ctx), event)
			}
			continue
		}

		// Already delivered via the OnElicitationRequest sink (Start) for
		// runtimes that mirror it onto RunStream too; skip that duplicate
		// copy to avoid a second dialog for the same request. Runtimes that
		// deliver ONLY via RunStream (e.g. RemoteRuntime) are unaffected —
		// mustSkipMirroredElicitation is false for them (#3584 review).
		if skipMirroredElicitation && isElicitationRequestEvent(event) {
			continue
		}

		if filter != nil && !filter(event) {
			continue
		}

		// Clear titleGenerating flag when title is generated (from server for remote runtime)
		if _, ok := event.(*runtime.SessionTitleEvent); ok {
			a.titleGenerating.Store(false)
		}

		a.sendEvent(ctx, event)
	}

	if !sawRootStop {
		a.synthesizeStreamStopped(ctx, a.session.ID, agentName, sawRootError)
	}
}

// isRootSessionEvent reports whether event belongs to the given root
// session rather than a sub-session (delegation or fork-skill child).
// Events that don't implement [runtime.SessionScoped], or that carry an
// empty SessionID, are treated as root-scoped by convention — matching how
// [runtime.SessionScoped] consumers elsewhere (e.g. the supervisor's
// isTopLevelStream) already interpret an absent session id.
func isRootSessionEvent(event runtime.Event, sessionID string) bool {
	scoped, ok := event.(runtime.SessionScoped)
	if !ok {
		return true
	}
	id := scoped.GetSessionID()
	return id == "" || id == sessionID
}

// synthesizeStreamStopped sends a StreamStoppedEvent for sessionID when the
// runtime's event channel closed without forwarding one (#4136). Consumers
// (the chat page, the supervisor) clear their "Working…" state only on
// receipt of this event and treat channel close as the terminal signal only
// in the runtime's documentation, not in their own code — so a dropped
// event (see LocalRuntime.finalizeEventChannel's non-blocking emit) leaves
// them stuck indefinitely without this fallback.
func (a *App) synthesizeStreamStopped(ctx context.Context, sessionID, agentName string, sawError bool) {
	reason := runtime.TurnEndReasonNormal
	switch {
	case ctx.Err() != nil:
		reason = runtime.TurnEndReasonCanceled
	case sawError:
		reason = runtime.TurnEndReasonError
	}
	if agentName == "" {
		agentName = a.runtime.CurrentAgentName(ctx)
	}
	slog.WarnContext(ctx, "runtime stream closed without a StreamStoppedEvent; synthesizing one",
		"session_id", sessionID, "reason", reason)
	// ctx may already be cancelled; detach cancellation but keep its trace
	// context so the synthesized stop still reaches subscribers, mirroring
	// the ctx-cancelled StreamStoppedEvent forwarding above.
	a.sendEvent(context.WithoutCancel(ctx), runtime.StreamStopped(sessionID, agentName, reason))
}

// acquireStreamGuard locks a.streamGuard (set via WithStreamGuard) and
// returns the matching release func, or a no-op release when no guard is
// attached (a bare App with no SessionManager to race against). Callers must
// hold the lock for the entire direct RunStream call — acquire it before
// mutating the session and defer the release only after the stream ends —
// mirroring the order SessionManager.RunSession already uses so a concurrent
// AddMessage/UpdateMessage/RunSession sees this stream as busy the same way
// (#3590).
func (a *App) acquireStreamGuard() func() {
	if a.streamGuard == nil {
		return func() {}
	}
	a.streamGuard.Lock()
	return a.streamGuard.Unlock
}

// processInlineAttachment handles content that is already in memory (e.g. pasted
// text). The content is appended to textBuilder wrapped in an XML tag for context.
func (a *App) processInlineAttachment(att messages.Attachment, textBuilder *strings.Builder) {
	textBuilder.WriteString("\n\n")
	fmt.Fprintf(textBuilder, "<attached_file path=%q>\n%s\n</attached_file>", att.Name, att.Content)
}

// Retry re-runs the agent loop on the current session without adding a new
// user message. It is used to resume the conversation after an error: the
// session already holds the messages exchanged so far, so RunStream picks up
// from where it left off.
//
// RunStream re-emits a UserMessageEvent for the trailing session message at
// startup (before StreamStarted) whenever SendUserMessage is set. On retry
// that message is already displayed, so forwarding the re-emission would
// duplicate the user bubble — or, when the tail is a tool/assistant message,
// render non-user content inside a spurious user bubble. We suppress any
// UserMessageEvent observed before StreamStarted to drop exactly that
// re-emission; genuine user messages injected mid-run (steer / follow-up)
// arrive after StreamStarted and are forwarded normally.
func (a *App) Retry(ctx context.Context, cancel context.CancelFunc) {
	a.cancel = cancel

	go func() {
		release := a.acquireStreamGuard()
		defer release()

		streamStarted := false
		a.forwardRunStreamEvents(ctx, a.runtime.RunStream(ctx, a.session), func(event runtime.Event) bool {
			switch event.(type) {
			case *runtime.StreamStartedEvent:
				streamStarted = true
			case *runtime.UserMessageEvent:
				if !streamStarted {
					return false
				}
			}
			return true
		})
	}()
}

// RunWithMessage runs the agent loop with a pre-constructed message.
// This is used for special cases like image attachments.
func (a *App) RunWithMessage(ctx context.Context, cancel context.CancelFunc, msg *session.Message) {
	a.cancel = cancel

	// If this is the first message and no title exists, start local title generation
	if a.session.TitleSnapshot() == "" && a.titleGen != nil {
		a.titleGenerating.Store(true)
		// Extract text content from the message for title generation
		userMessage := msg.Message.Content
		if userMessage == "" && len(msg.Message.MultiContent) > 0 {
			for _, part := range msg.Message.MultiContent {
				if part.Type == chat.MessagePartTypeText {
					userMessage = part.Text
					break
				}
			}
		}
		go a.generateTitle(ctx, []string{userMessage})
	}

	go func() {
		release := a.acquireStreamGuard()
		defer release()

		a.session.AddMessage(msg)
		a.forwardRunStreamEvents(ctx, a.runtime.RunStream(ctx, a.session), nil)
	}()
}

func (a *App) RunBangCommand(ctx context.Context, command string) {
	command = strings.TrimSpace(command)
	if command == "" {
		a.events <- runtime.ShellOutput("Error: empty command")
		return
	}

	shell, argsPrefix := shellpath.DetectShell()
	out, err := exec.CommandContext(ctx, shell, append(argsPrefix, command)...).CombinedOutput()
	output := "$ " + command + "\n" + string(out)
	if err != nil && len(out) == 0 {
		output = "$ " + command + "\nError: " + err.Error()
	}
	a.events <- runtime.ShellOutput(output)
}

// InjectUserMessage feeds content into the app exactly as if the user had
// typed and submitted it in the TUI. It is the entry point external drivers
// (the --listen control plane) use to send follow-up prompts: routing through
// the normal SendMsg path means the message is queued when the agent is busy,
// triggers title generation on the first turn, and — crucially — starts a
// RunStream whose events flow through a.events to every subscriber (the TUI
// and any SSE consumer). Enqueuing into the runtime's follow-up queue instead
// would do nothing while the agent is idle, since that queue is only drained
// mid-stream.
//
// SendMsg is a TUI message, not a runtime.Event, so SSE subscribers (which
// forward only runtime.Events) ignore it; it reaches the TUI program alone.
func (a *App) InjectUserMessage(ctx context.Context, content string) {
	a.sendEvent(ctx, messages.SendMsg{Content: content})
}

// SubscribeWith subscribes to app events using a custom send function.
// Multiple concurrent subscribers are supported: a single fan-out goroutine
// drains the throttled event stream and dispatches a copy to each one.
// Slow subscribers drop events rather than block the bus.
func (a *App) SubscribeWith(ctx context.Context, send func(tea.Msg)) {
	ch := make(chan tea.Msg, subscriberBufferSize)
	a.addSubscriber(ch)
	defer a.removeSubscriber(ch)

	a.fanoutOnce.Do(a.startFanOut)

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			send(msg)
		}
	}
}

const subscriberBufferSize = 1024

func (a *App) addSubscriber(ch chan tea.Msg) {
	a.subsMu.Lock()
	defer a.subsMu.Unlock()
	a.subs = append(a.subs, ch)
}

func (a *App) removeSubscriber(ch chan tea.Msg) {
	a.subsMu.Lock()
	defer a.subsMu.Unlock()
	a.subs = slices.DeleteFunc(a.subs, func(c chan tea.Msg) bool { return c == ch })
}

// startFanOut runs once per App. It throttles the raw events channel and
// scatters every message to all currently-registered subscribers. Sends are
// non-blocking; if a subscriber's buffer is full the event is dropped for
// that subscriber so one slow consumer cannot stall the others.
//
// Turn-boundary events are the exception: dropping a stream_started or
// stream_stopped skews a consumer's turn accounting for good (the SSE replay
// buffer never sees the event, so reconnecting cannot recover it). For those,
// the oldest pending message — almost always a content delta, which the next
// delta supersedes — is evicted to make room instead.
func (a *App) startFanOut() {
	throttled := a.throttleEvents(a.ctx(), a.events)
	go func() {
		for msg := range throttled {
			a.subsMu.Lock()
			subs := slices.Clone(a.subs)
			a.subsMu.Unlock()
			for _, ch := range subs {
				select {
				case ch <- msg:
				default:
					if !isTurnBoundaryEvent(msg) {
						slog.Warn("app: subscriber buffer full, dropping event")
						continue
					}
					// Evict the oldest pending message (racing the subscriber's
					// own receive is fine: either way a slot frees up), then
					// retry once. Still full means the subscriber is wedged;
					// drop as before rather than block the fan-out.
					select {
					case <-ch:
					default:
					}
					select {
					case ch <- msg:
					default:
						slog.Warn("app: subscriber buffer full, dropping turn-boundary event")
					}
				}
			}
		}
	}()
}

// isTurnBoundaryEvent reports whether msg is one of the events consumers use
// to track turn state (running/waiting/failed/paused) and identity (title).
// These are low-frequency and irrecoverable when lost, unlike the content
// deltas that dominate the stream, so the fan-out prefers them on overflow.
func isTurnBoundaryEvent(msg tea.Msg) bool {
	switch msg.(type) {
	case *runtime.StreamStartedEvent,
		*runtime.StreamStoppedEvent,
		*runtime.UserMessageEvent,
		*runtime.ErrorEvent,
		*runtime.PausedEvent,
		*runtime.SessionTitleEvent:
		return true
	default:
		return false
	}
}

// Resume resumes the runtime with the given confirmation request
func (a *App) Resume(req runtime.ResumeRequest) {
	a.runtime.Resume(a.ctx(), req)
}

// Steer queues a user message for mid-turn injection into the running agent.
func (a *App) Steer(ctx context.Context, msg runtime.QueuedMessage) error {
	return a.runtime.Steer(ctx, msg)
}

// FollowUp queues a user message for a separate turn after the current turn.
func (a *App) FollowUp(ctx context.Context, msg runtime.QueuedMessage) error {
	return a.runtime.FollowUp(ctx, msg)
}

// SteerMessage resolves attachments into message parts and queues the result
// for mid-turn injection into the running agent. The runtime appends the
// message to the session (and emits the matching UserMessageEvent) when the
// agent loop drains it.
func (a *App) SteerMessage(ctx context.Context, content string, attachments []messages.Attachment) error {
	msg := runtime.QueuedMessage{Content: content}
	if len(attachments) > 0 {
		msg.MultiContent = a.buildUserMultiContent(ctx, content, attachments)
	}
	return a.runtime.Steer(ctx, msg)
}

// FollowUpMessage resolves attachments and queues a message for a separate turn
// after the current agent turn finishes.
func (a *App) FollowUpMessage(ctx context.Context, content string, attachments []messages.Attachment) error {
	msg := runtime.QueuedMessage{Content: content}
	if len(attachments) > 0 {
		msg.MultiContent = a.buildUserMultiContent(ctx, content, attachments)
	}
	return a.runtime.FollowUp(ctx, msg)
}

// TogglePause toggles whether the runtime loop is paused at iteration
// boundaries. The second return value is false if the underlying runtime
// doesn't support pausing (e.g. remote runtimes), in which case the first
// return value is meaningless.
func (a *App) TogglePause() (paused, supported bool) {
	p, err := a.runtime.TogglePause(a.ctx())
	if errors.Is(err, runtime.ErrUnsupported) {
		return false, false
	}
	if err != nil {
		slog.Error("Failed to toggle pause", "error", err)
		return false, false
	}
	return p, true
}

// ResumeElicitation resumes an elicitation request with the given action and
// content. elicitationID is variadic, mirroring runtime.Runtime.ResumeElicitation,
// purely so pre-#3584 3-arg callers keep compiling unchanged; at most the
// first value is meaningful, and "" (or omitting it) falls back to resolving
// the sole pending request.
func (a *App) ResumeElicitation(ctx context.Context, action tools.ElicitationAction, content map[string]any, elicitationID ...string) error {
	return a.runtime.ResumeElicitation(ctx, action, content, elicitationID...)
}

func (a *App) NewSession() {
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	// Preserve user-controlled session flags
	// so they don't reset to default on /new
	var opts []session.Opt
	var priorPolicy session.SafetyPolicy
	if a.session != nil {
		// WithSafetyPolicy("") is a no-op, so a legacy session (no explicit
		// mode) keeps relying on the ToolsApproved flag alone; an explicit
		// strict/balanced choice — which leaves ToolsApproved=false — must be
		// carried over here or /new silently falls back to legacy behavior.
		opts = append(opts,
			session.WithToolsApproved(a.session.ToolsApproved),
			session.WithSafetyPolicy(a.session.GetSafetyPolicy()),
			session.WithHideToolResults(a.session.HideToolResults),
			session.WithWorkingDir(a.session.WorkingDir),
		)
		priorPolicy = a.session.GetPriorSafetyPolicy()
	}
	// There is no WithPriorSafetyPolicy option; writing the field before the
	// session is published via a.session keeps the write on an unshared
	// value, so it is safe. Preserving the toggle memory keeps an autonomous
	// escalation able to toggle back to its prior mode.
	sess := session.New(opts...)
	sess.PriorSafetyPolicy = priorPolicy
	a.session = sess
	// Clear first message so it won't be re-sent on re-init
	a.firstMessage = nil
	a.firstMessageAttach = ""

	// Re-emit startup info so the sidebar shows agent/tools info in the new session
	a.reEmitStartupInfo(a.ctx())
}

// reEmitStartupInfo resets and re-emits startup info (agent, team, tools)
// through the events channel so the sidebar updates.
func (a *App) reEmitStartupInfo(ctx context.Context) {
	a.runtime.ResetStartupInfo()
	a.pumpToEvents(ctx, func(sink runtime.EventSink) {
		a.runtime.EmitStartupInfo(ctx, a.session, sink)
	})
}

// pumpToEvents runs emit (which produces events into the supplied sink) in a
// background goroutine and forwards everything it emits to the app's events
// channel, without blocking the caller.
func (a *App) pumpToEvents(ctx context.Context, emit func(runtime.EventSink)) {
	go func() {
		ch := make(chan runtime.Event, 10)
		go func() {
			defer close(ch)
			emit(runtime.NewChannelSink(ch))
		}()
		for event := range ch {
			select {
			case a.events <- event:
			case <-ctx.Done():
				return
			default:
			}
		}
	}()
}

func (a *App) Session() *session.Session {
	return a.session
}

// contextBreakdownProvider is an optional runtime capability: computing the
// estimated context-window composition for a session. Only the local runtime
// (which holds the agent and its tools) implements it; remote runtimes
// don't, so the /context dialog reports the feature as unavailable.
type contextBreakdownProvider interface {
	ContextBreakdown(ctx context.Context, sess *session.Session) (*runtime.ContextBreakdown, error)
}

// generatedFileResolver is an optional runtime capability: resolving one
// recorded generated-media reference to its bytes and validated canonical
// path, gated on the generated-media manifest and the owning session's
// workspace (see [runtime.LocalRuntime.ResolveGeneratedFile]). Only the
// local runtime implements it; remote runtimes never deliver generated-file
// payloads, so UIs treat the missing capability as "nothing to resolve".
type generatedFileResolver interface {
	ResolveGeneratedFile(ctx context.Context, ref runtime.GeneratedFileRef) (*runtime.ResolvedGeneratedFile, error)
}

// CanResolveGeneratedFiles reports whether the runtime can resolve
// generated-media references at all, letting UIs skip resolution work
// entirely on runtimes without the capability.
func (a *App) CanResolveGeneratedFiles() bool {
	_, ok := a.runtime.(generatedFileResolver)
	return ok
}

// ResolveGeneratedFile resolves one recorded generated-media reference.
// Returns an error wrapping [runtime.ErrUnsupported] when the runtime does
// not own local generated media (e.g. remote runtimes). Callers must treat
// any error as "unavailable" — never surface its text to the user.
func (a *App) ResolveGeneratedFile(ctx context.Context, ref runtime.GeneratedFileRef) (*runtime.ResolvedGeneratedFile, error) {
	resolver, ok := a.runtime.(generatedFileResolver)
	if !ok {
		return nil, fmt.Errorf("generated file resolution: %w", runtime.ErrUnsupported)
	}
	return resolver.ResolveGeneratedFile(ctx, ref)
}

// ContextBreakdown returns the estimated context-window composition for the
// current session. Returns an error wrapping [runtime.ErrUnsupported] when
// the runtime cannot compute it (e.g. remote runtimes).
func (a *App) ContextBreakdown(ctx context.Context) (*runtime.ContextBreakdown, error) {
	cp, ok := a.runtime.(contextBreakdownProvider)
	if !ok {
		return nil, fmt.Errorf("context breakdown: %w", runtime.ErrUnsupported)
	}
	return cp.ContextBreakdown(ctx, a.Session())
}

// liveSessionLister is an optional runtime capability: listing the current
// root session plus every live sub-agent session for the /context team view.
// Only the local runtime (which owns the RunStream registry) implements it;
// remote runtimes degrade to a root-only view.
type liveSessionLister interface {
	LiveSessions(ctx context.Context, current *session.Session) []runtime.LiveSession
}

// LiveSessions returns the current root session plus every currently live
// sub-agent session (foreground children and background agent tasks), or nil
// when the runtime does not expose live-session tracking.
func (a *App) LiveSessions(ctx context.Context) []runtime.LiveSession {
	lister, ok := a.runtime.(liveSessionLister)
	if !ok {
		return nil
	}
	return lister.LiveSessions(ctx, a.Session())
}

// liveSessionCompactor is an optional runtime capability: queueing an
// explicit compaction of one live session onto that session's own run loop.
type liveSessionCompactor interface {
	CompactLiveSession(ctx context.Context, sessionID, additionalPrompt string, events runtime.EventSink) error
}

// CompactLiveSession queues a manual compaction for the identified live
// session and bridges the resulting compaction/usage events into the app's
// event stream. The request executes on the target session's own run loop at
// a safe iteration boundary; neither the root stream nor the target stream
// is cancelled. Returns an error wrapping [runtime.ErrUnsupported] when the
// runtime cannot target live sessions (e.g. remote runtimes), or the
// runtime's rejection for unknown/finished sessions and duplicate requests.
func (a *App) CompactLiveSession(ctx context.Context, sessionID, additionalPrompt string) error {
	compactor, ok := a.runtime.(liveSessionCompactor)
	if !ok {
		return fmt.Errorf("targeted session compaction: %w", runtime.ErrUnsupported)
	}
	sink := runtime.EventSinkFunc(func(event runtime.Event) {
		a.sendEvent(ctx, event)
	})
	return compactor.CompactLiveSession(ctx, sessionID, additionalPrompt, sink)
}

// AttachedFiles returns the current session's attached file paths, the same
// inventory DropAttachedFile resolves against. Backs /drop argument
// completion; nil when there is no active session. Attachments are
// in-memory client-side session state, so this works on remote runtimes too.
func (a *App) AttachedFiles() []string {
	sess := a.Session()
	if sess == nil {
		return nil
	}
	return sess.AttachedFilesSnapshot()
}

// DropAttachedFile removes a file from the current session's attached files
// and returns the absolute path that was dropped. The path is resolved
// against the session's attachment list: exact match first, then the
// absolute form of a relative path, then a unique base-name match. Dropping
// stops the file from being propagated to future sub-agent delegations and
// skill prompts; content already inlined in past messages is unaffected.
//
// Attached files are in-memory session state (recording them never touches
// the store either), so the store sync afterwards is best-effort: it only
// refreshes stores that snapshot the attachment list and a failure does not
// undo the drop.
func (a *App) DropAttachedFile(ctx context.Context, path string) (string, error) {
	sess := a.Session()
	if sess == nil {
		return "", errors.New("no active session")
	}
	resolved, err := resolveAttachedFile(sess.AttachedFilesSnapshot(), path)
	if err != nil {
		return "", err
	}
	if !sess.RemoveAttachedFile(resolved) {
		return "", fmt.Errorf("file is not attached to this session: %s", resolved)
	}
	if store := a.runtime.SessionStore(); store != nil {
		if err := store.UpdateSession(ctx, sess); err != nil {
			slog.WarnContext(ctx, "Failed to sync session store after dropping attachment", "session_id", sess.ID, "path", resolved, "error", err)
		}
	}
	return resolved, nil
}

// resolveAttachedFile maps user input to one recorded attachment path.
func resolveAttachedFile(attached []string, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("no file specified")
	}
	if len(attached) == 0 {
		return "", errors.New("no files are attached to this session")
	}
	if slices.Contains(attached, path) {
		return path, nil
	}
	if abs, err := filepath.Abs(path); err == nil && slices.Contains(attached, abs) {
		return abs, nil
	}
	var matches []string
	for _, candidate := range attached {
		if filepath.Base(candidate) == path {
			matches = append(matches, candidate)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("file is not attached to this session: %s", path)
	default:
		return "", fmt.Errorf("%q matches %d attached files, use the full path", path, len(matches))
	}
}

// PermissionsInfo returns combined permissions info from team and session.
// Returns nil if no permissions are configured at either level.
func (a *App) PermissionsInfo() *runtime.PermissionsInfo {
	// Get team-level permissions from runtime
	teamPerms := a.runtime.PermissionsInfo()

	// Get session-level permissions
	var sessionPerms *runtime.PermissionsInfo
	if a.session != nil && a.session.Permissions != nil {
		if len(a.session.Permissions.Allow) > 0 || len(a.session.Permissions.Ask) > 0 || len(a.session.Permissions.Deny) > 0 {
			sessionPerms = &runtime.PermissionsInfo{
				Allow: a.session.Permissions.Allow,
				Ask:   a.session.Permissions.Ask,
				Deny:  a.session.Permissions.Deny,
			}
		}
	}

	// Return nil if no permissions configured at any level
	if teamPerms == nil && sessionPerms == nil {
		return nil
	}

	// Merge permissions, with session taking priority (listed first)
	result := &runtime.PermissionsInfo{}
	if sessionPerms != nil {
		result.Allow = append(result.Allow, sessionPerms.Allow...)
		result.Ask = append(result.Ask, sessionPerms.Ask...)
		result.Deny = append(result.Deny, sessionPerms.Deny...)
	}
	if teamPerms != nil {
		result.Allow = append(result.Allow, teamPerms.Allow...)
		result.Ask = append(result.Ask, teamPerms.Ask...)
		result.Deny = append(result.Deny, teamPerms.Deny...)
	}

	return result
}

// HasPermissions returns true if any permissions are configured (team or session level).
func (a *App) HasPermissions() bool {
	return a.PermissionsInfo() != nil
}

// SwitchAgent switches the currently active agent for subsequent user messages
func (a *App) SwitchAgent(agentName string) error {
	// Called from the Bubble Tea event loop, which has no context; this is
	// a TUI-root boundary.
	return a.runtime.SetCurrentAgent(a.ctx(), agentName)
}

// SetCurrentAgentModel sets the model for the current agent and persists
// the override in the session. Returns an error if model switching is not
// supported by the runtime (e.g., remote runtimes).
// Pass an empty modelRef to clear the override and use the agent's default model.
func (a *App) SetCurrentAgentModel(ctx context.Context, modelRef string) error {
	agentName := a.runtime.CurrentAgentName(ctx)

	// Set the model override on the runtime (empty modelRef clears the override)
	if err := a.runtime.SetAgentModel(ctx, agentName, modelRef); err != nil {
		if errors.Is(err, runtime.ErrUnsupported) {
			return errors.New("model switching not supported by this runtime")
		}
		return err
	}

	// Update the session's model overrides
	if modelRef == "" {
		// Clear the override - remove from map
		delete(a.session.AgentModelOverrides, agentName)
		slog.DebugContext(ctx, "Cleared model override from session", "session_id", a.session.ID, "agent", agentName)
	} else {
		// Set the override
		if a.session.AgentModelOverrides == nil {
			a.session.AgentModelOverrides = make(map[string]string)
		}
		a.session.AgentModelOverrides[agentName] = modelRef
		slog.DebugContext(ctx, "Set model override in session", "session_id", a.session.ID, "agent", agentName, "model", modelRef)

		// Track custom models (inline provider/model format) in the session
		if strings.Contains(modelRef, "/") {
			a.trackCustomModel(modelRef)
		}
	}

	// Persist the session
	if store := a.runtime.SessionStore(); store != nil {
		if err := store.UpdateSession(ctx, a.session); err != nil {
			return fmt.Errorf("failed to persist model override: %w", err)
		}
		slog.DebugContext(ctx, "Persisted session with model override", "session_id", a.session.ID, "overrides", a.session.AgentModelOverrides)
	}

	// Re-emit startup info so the sidebar updates with the new model
	a.reEmitStartupInfo(ctx)

	return nil
}

// CycleAgentThinkingLevel advances the current agent's thinking-effort level
// to the next value in its provider's cycle and returns the newly selected
// level. The change applies to the running session only (it is not persisted
// as a model override). Returns an error wrapping [runtime.ErrUnsupported]
// when the runtime or model cannot cycle thinking levels.
func (a *App) CycleAgentThinkingLevel(ctx context.Context) (effort.Level, error) {
	agentName := a.runtime.CurrentAgentName(ctx)
	level, err := a.runtime.CycleAgentThinkingLevel(ctx, agentName)
	if err != nil {
		return "", err
	}
	// Refresh only the agent/team info so the sidebar reflects the updated
	// thinking level. We intentionally avoid reEmitStartupInfo here: it would
	// re-run tool discovery and make the sidebar's tool count flicker.
	a.refreshAgentInfo(ctx)
	return level, nil
}

// SetAgentThinkingLevel sets the current agent's thinking-effort level to the
// requested value and returns the applied level. Like
// [App.CycleAgentThinkingLevel], the change applies to the running session
// only. Returns an error wrapping [runtime.ErrUnsupported] when the runtime
// or model cannot switch thinking levels.
func (a *App) SetAgentThinkingLevel(ctx context.Context, level effort.Level) (effort.Level, error) {
	agentName := a.runtime.CurrentAgentName(ctx)
	applied, err := a.runtime.SetAgentThinkingLevel(ctx, agentName, level)
	if err != nil {
		return "", err
	}
	a.refreshAgentInfo(ctx)
	return applied, nil
}

// refreshAgentInfo pushes fresh agent and team info through the events channel
// without re-running the heavier startup steps (tool discovery, token usage).
func (a *App) refreshAgentInfo(ctx context.Context) {
	a.pumpToEvents(ctx, func(sink runtime.EventSink) {
		a.runtime.EmitAgentInfo(ctx, sink)
	})
}

// RefreshModelsCatalog forces a refetch of the models.dev catalog that
// backs catalog-driven model discovery. Refreshing is an optional
// runtime capability: only the local runtime exposes it, so remote
// runtimes report [runtime.ErrUnsupported].
func (a *App) RefreshModelsCatalog(ctx context.Context) error {
	refresher, ok := a.runtime.(interface {
		RefreshModelsCatalog(ctx context.Context) error
	})
	if !ok {
		return runtime.ErrUnsupported
	}
	return refresher.RefreshModelsCatalog(ctx)
}

// AvailableModels returns the list of models available for selection.
// Returns nil if model switching is not supported.
func (a *App) AvailableModels(ctx context.Context) []runtime.ModelChoice {
	start := time.Now()
	if !a.runtime.SupportsModelSwitching() {
		slog.DebugContext(ctx, "App available models skipped; model switching unsupported", "duration", time.Since(start))
		return nil
	}

	agentName := a.runtime.CurrentAgentName(ctx)
	currentRef := ""
	var customRefs []string
	if a.session != nil {
		currentRef = a.session.AgentModelOverrides[agentName]
		customRefs = a.session.CustomModelsUsed
	}

	runtimeStart := time.Now()
	baseModels := a.runtime.AvailableModels(ctx)
	runtimeDuration := time.Since(runtimeStart)
	decorateStart := time.Now()
	models := runtime.DecorateModelChoices(baseModels, currentRef, customRefs)
	slog.DebugContext(ctx, "App available models completed",
		"duration", time.Since(start),
		"runtime_duration", runtimeDuration,
		"decorate_duration", time.Since(decorateStart),
		"base_models", len(baseModels),
		"models", len(models),
		"custom_refs", len(customRefs),
		"agent", agentName,
	)
	return models
}

// trackCustomModel adds a custom model to the session's history if not already present.
func (a *App) trackCustomModel(modelRef string) {
	if a.session == nil {
		return
	}

	// Check if already tracked
	if slices.Contains(a.session.CustomModelsUsed, modelRef) {
		return
	}

	a.session.CustomModelsUsed = append(a.session.CustomModelsUsed, modelRef)
	slog.Debug("Tracked custom model in session", "session_id", a.session.ID, "model", modelRef)
}

// SupportsModelSwitching returns true if the runtime supports model switching.
func (a *App) SupportsModelSwitching() bool {
	return a.runtime.SupportsModelSwitching()
}

// ShouldExitAfterFirstResponse returns true if the app is configured to exit
// after the first assistant response completes.
func (a *App) ShouldExitAfterFirstResponse() bool {
	return a.exitAfterFirstResponse
}

// IsReadOnly returns true when the session is in read-only mode and no new
// messages should be sent to the LLM.
func (a *App) IsReadOnly() bool {
	return a.readOnly
}

func (a *App) CompactSession(ctx context.Context, cancel context.CancelFunc, additionalPrompt string) {
	a.cancel = cancel

	sess := a.session
	if sess == nil {
		cancel()
		return
	}

	go func() {
		defer cancel()

		agentName := a.runtime.CurrentAgentName(ctx)
		completed := false
		events := make(chan runtime.Event, 100)
		go func() {
			defer close(events)
			a.runtime.Summarize(ctx, sess, additionalPrompt, runtime.NewChannelSink(events))
		}()
		for event := range events {
			if ctx.Err() != nil {
				return
			}
			if e, ok := event.(*runtime.SessionCompactionEvent); ok && e.Status == "completed" {
				completed = true
			}
			a.sendEvent(ctx, event)
		}
		if !completed && ctx.Err() == nil {
			// The compaction never emitted its started/completed pair (e.g. a
			// pre_compact / before_compaction hook vetoed it). Synthesize the
			// terminal event so the TUI unsticks, but report it as skipped —
			// nothing was compacted.
			a.sendEvent(ctx, runtime.SessionCompactionCompleted(sess.ID, runtime.CompactionOutcomeSkipped, agentName))
		}
	}()
}

func (a *App) PlainTextTranscript() string {
	return transcript.PlainText(a.session)
}

// SessionStore returns the session store for browsing/loading sessions.
// Returns nil if no session store is configured.
func (a *App) SessionStore() session.Store {
	return a.runtime.SessionStore()
}

// ReplaceSession replaces the current session with the given session.
// This is used when loading a past session. It also re-emits startup info
// so the sidebar displays the agent and tool information.
// If the session has stored model overrides, they are applied to the runtime.
func (a *App) ReplaceSession(ctx context.Context, sess *session.Session) {
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.session = sess
	// Clear first message so it won't be re-sent on re-init
	a.firstMessage = nil
	a.firstMessageAttach = ""

	// Apply any stored model overrides from the session
	a.applySessionModelOverrides(ctx, sess)

	// Reset and re-emit startup info so the sidebar shows agent/tools info
	a.reEmitStartupInfo(ctx)
}

// applySessionModelOverrides applies any stored model overrides from a loaded session.
func (a *App) applySessionModelOverrides(ctx context.Context, sess *session.Session) {
	if len(sess.AgentModelOverrides) == 0 {
		slog.DebugContext(ctx, "No model overrides to apply from session", "session_id", sess.ID)
		return
	}

	// Check if runtime supports model switching
	if !a.runtime.SupportsModelSwitching() {
		slog.DebugContext(ctx, "Runtime does not support model switching, skipping overrides")
		return
	}

	slog.DebugContext(ctx, "Applying model overrides from session", "session_id", sess.ID, "overrides", sess.AgentModelOverrides)
	for agentName, modelRef := range sess.AgentModelOverrides {
		if err := a.runtime.SetAgentModel(ctx, agentName, modelRef); err != nil {
			// Log but don't fail - the session can still be used with default models
			slog.WarnContext(ctx, "Failed to apply model override from session", "agent", agentName, "model", modelRef, "error", err)
			a.events <- runtime.Warning(fmt.Sprintf("Failed to apply model override for agent %q: %v", agentName, err), agentName)
		} else {
			slog.InfoContext(ctx, "Applied model override from session", "agent", agentName, "model", modelRef)
		}
	}
}

// throttleEvents buffers and merges rapid events to prevent UI flooding
func (a *App) throttleEvents(ctx context.Context, in <-chan tea.Msg) <-chan tea.Msg {
	out := make(chan tea.Msg, 128)

	go func() {
		defer close(out)

		var buffer []tea.Msg
		var timerCh <-chan time.Time

		flush := func() {
			for _, msg := range a.mergeEvents(buffer) {
				select {
				case out <- msg:
				case <-ctx.Done():
					return
				}
			}
			buffer = buffer[:0]
			timerCh = nil
		}

		for {
			select {
			case <-ctx.Done():
				return

			case msg, ok := <-in:
				if !ok {
					return
				}

				buffer = append(buffer, msg)
				if a.shouldThrottle(msg) {
					if timerCh == nil {
						timerCh = time.After(a.throttleDuration)
					}
				} else {
					flush()
				}

			case <-timerCh:
				flush()
			}
		}
	}()

	return out
}

// shouldThrottle determines if an event should be buffered/throttled
func (a *App) shouldThrottle(msg tea.Msg) bool {
	switch msg.(type) {
	case *runtime.AgentChoiceEvent:
		return true
	case *runtime.AgentChoiceReasoningEvent:
		return true
	case *runtime.PartialToolCallEvent:
		return true
	case *runtime.ToolCallOutputEvent:
		return true
	default:
		return false
	}
}

// mergeEvents merges consecutive similar events to reduce UI updates.
//
// Each merge group is built with a single strings.Builder so concatenating N
// chunks costs O(N) instead of the O(N^2) the naive `merged.Content + next.Content`
// pattern produces. This matters during fast LLM streams where dozens of
// chunks land per throttle window.
func (a *App) mergeEvents(events []tea.Msg) []tea.Msg {
	if len(events) == 0 {
		return events
	}

	result := make([]tea.Msg, 0, len(events))

	for i := 0; i < len(events); i++ {
		switch ev := events[i].(type) {
		case *runtime.AgentChoiceEvent:
			merged, consumed := mergeAgentChoiceRun(ev, events[i+1:])
			result = append(result, merged)
			i += consumed

		case *runtime.AgentChoiceReasoningEvent:
			merged, consumed := mergeAgentChoiceReasoningRun(ev, events[i+1:])
			result = append(result, merged)
			i += consumed

		case *runtime.PartialToolCallEvent:
			merged, consumed := mergePartialToolCallRun(ev, events[i+1:])
			result = append(result, merged)
			i += consumed

		case *runtime.ToolCallOutputEvent:
			merged, consumed := mergeToolCallOutputRun(ev, events[i+1:])
			result = append(result, merged)
			i += consumed

		default:
			result = append(result, events[i])
		}
	}

	return result
}

// mergeAgentChoiceRun merges first with any directly-following AgentChoiceEvents
// for the same agent. It returns the merged event and the number of follow-up
// events that were consumed.
func mergeAgentChoiceRun(first *runtime.AgentChoiceEvent, rest []tea.Msg) (*runtime.AgentChoiceEvent, int) {
	n := 0
	total := len(first.Content)
	for _, msg := range rest {
		next, ok := msg.(*runtime.AgentChoiceEvent)
		if !ok || next.AgentName != first.AgentName {
			break
		}
		total += len(next.Content)
		n++
	}
	if n == 0 {
		return first, 0
	}

	var b strings.Builder
	b.Grow(total)
	b.WriteString(first.Content)
	for _, msg := range rest[:n] {
		b.WriteString(msg.(*runtime.AgentChoiceEvent).Content)
	}
	return &runtime.AgentChoiceEvent{
		Type:         first.Type,
		Content:      b.String(),
		AgentContext: first.AgentContext,
	}, n
}

// mergeAgentChoiceReasoningRun is the AgentChoiceReasoningEvent counterpart of
// mergeAgentChoiceRun.
func mergeAgentChoiceReasoningRun(first *runtime.AgentChoiceReasoningEvent, rest []tea.Msg) (*runtime.AgentChoiceReasoningEvent, int) {
	n := 0
	total := len(first.Content)
	for _, msg := range rest {
		next, ok := msg.(*runtime.AgentChoiceReasoningEvent)
		if !ok || next.AgentName != first.AgentName {
			break
		}
		total += len(next.Content)
		n++
	}
	if n == 0 {
		return first, 0
	}

	var b strings.Builder
	b.Grow(total)
	b.WriteString(first.Content)
	for _, msg := range rest[:n] {
		b.WriteString(msg.(*runtime.AgentChoiceReasoningEvent).Content)
	}
	return &runtime.AgentChoiceReasoningEvent{
		Type:         first.Type,
		Content:      b.String(),
		AgentContext: first.AgentContext,
	}, n
}

// mergeToolCallOutputRun merges output chunks across consecutive
// ToolCallOutputEvents that share the same tool call ID.
func mergeToolCallOutputRun(first *runtime.ToolCallOutputEvent, rest []tea.Msg) (*runtime.ToolCallOutputEvent, int) {
	n := 0
	total := len(first.Output)
	for _, msg := range rest {
		next, ok := msg.(*runtime.ToolCallOutputEvent)
		if !ok || next.ToolCallID != first.ToolCallID {
			break
		}
		total += len(next.Output)
		n++
	}
	if n == 0 {
		return first, 0
	}

	var b strings.Builder
	b.Grow(total)
	b.WriteString(first.Output)
	for _, msg := range rest[:n] {
		b.WriteString(msg.(*runtime.ToolCallOutputEvent).Output)
	}
	return &runtime.ToolCallOutputEvent{
		Type:           first.Type,
		ToolCallID:     first.ToolCallID,
		ToolDefinition: first.ToolDefinition,
		Output:         b.String(),
		AgentContext:   first.AgentContext,
	}, n
}

// mergePartialToolCallRun merges argument deltas across consecutive
// PartialToolCallEvents that share the same tool call ID.
func mergePartialToolCallRun(first *runtime.PartialToolCallEvent, rest []tea.Msg) (*runtime.PartialToolCallEvent, int) {
	n := 0
	total := len(first.ToolCall.Function.Arguments)
	for _, msg := range rest {
		next, ok := msg.(*runtime.PartialToolCallEvent)
		if !ok || next.ToolCall.ID != first.ToolCall.ID {
			break
		}
		total += len(next.ToolCall.Function.Arguments)
		n++
	}
	if n == 0 {
		return first, 0
	}

	var b strings.Builder
	b.Grow(total)
	b.WriteString(first.ToolCall.Function.Arguments)

	name := first.ToolCall.Function.Name
	toolDef := first.ToolDefinition
	for _, msg := range rest[:n] {
		next := msg.(*runtime.PartialToolCallEvent)
		b.WriteString(next.ToolCall.Function.Arguments)
		// The function name is sometimes only present on later deltas; keep the
		// first non-empty value we observe across the run.
		name = cmp.Or(name, next.ToolCall.Function.Name)
		toolDef = cmp.Or(toolDef, next.ToolDefinition)
	}
	return &runtime.PartialToolCallEvent{
		Type: first.Type,
		ToolCall: tools.ToolCall{
			ID:   first.ToolCall.ID,
			Type: first.ToolCall.Type,
			Function: tools.FunctionCall{
				Name:      name,
				Arguments: b.String(),
			},
		},
		ToolDefinition: toolDef,
		AgentContext:   first.AgentContext,
	}, n
}

// ExportHTML exports the current session as a standalone HTML file.
// If filename is empty, a default name based on the session title and timestamp is used.
func (a *App) ExportHTML(ctx context.Context, filename string) (string, error) {
	agentInfo := a.runtime.CurrentAgentInfo(ctx)
	return export.SessionToFile(a.session, agentInfo.Description, filename)
}

// ErrTitleGenerating is returned when attempting to set a title while generation is in progress.
var ErrTitleGenerating = errors.New("title generation in progress, please wait")

// UpdateSessionTitle updates the current session's title and persists it.
// It works with both local and remote runtimes.
func (a *App) UpdateSessionTitle(ctx context.Context, title string) error {
	if a.session == nil {
		return errors.New("no active session")
	}

	// Prevent manual title edits while generation is in progress
	if a.titleGenerating.Load() {
		return ErrTitleGenerating
	}

	// Persist the title through the runtime
	if err := a.runtime.UpdateSessionTitle(ctx, a.session, title); err != nil {
		return fmt.Errorf("failed to update session title: %w", err)
	}

	// Emit a SessionTitleEvent to update the UI consistently
	a.events <- runtime.SessionTitle(a.session.ID, title)
	return nil
}

// IsTitleGenerating returns true if title generation is currently in progress.
func (a *App) IsTitleGenerating() bool {
	return a.titleGenerating.Load()
}

// generateTitle generates a title using the local title generator.
// This method always clears the titleGenerating flag when done (success or failure).
// It should be called in a goroutine.
func (a *App) generateTitle(ctx context.Context, userMessages []string) {
	// Always clear the flag when done, whether success or failure
	defer a.titleGenerating.Store(false)

	if a.titleGen == nil {
		slog.DebugContext(ctx, "No title generator available, skipping title generation")
		// Emit empty title event so the UI clears any title-generation spinner
		select {
		case a.events <- runtime.SessionTitle(a.session.ID, ""):
		case <-ctx.Done():
		}
		return
	}

	title, err := a.titleGen.Generate(ctx, a.session.ID, userMessages)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to generate session title", "session_id", a.session.ID, "error", err)
		// Emit empty title event so the UI clears any title-generation spinner
		select {
		case a.events <- runtime.SessionTitle(a.session.ID, ""):
		case <-ctx.Done():
		}
		return
	}

	if title == "" {
		// Emit empty title event so the UI clears any title-generation spinner
		select {
		case a.events <- runtime.SessionTitle(a.session.ID, ""):
		case <-ctx.Done():
		}
		return
	}

	// Persist the title
	if err := a.runtime.UpdateSessionTitle(ctx, a.session, title); err != nil {
		slog.ErrorContext(ctx, "Failed to persist title", "session_id", a.session.ID, "error", err)
	}

	// Emit the title event to update the UI
	select {
	case a.events <- runtime.SessionTitle(a.session.ID, title):
	case <-ctx.Done():
	}
}

// RegenerateSessionTitle triggers AI-based title regeneration for the current session.
// Returns ErrTitleGenerating if a title generation is already in progress.
func (a *App) RegenerateSessionTitle(ctx context.Context) error {
	if a.session == nil {
		return errors.New("no active session")
	}

	// Check if title generation is already in progress
	if a.titleGenerating.Load() {
		return ErrTitleGenerating
	}

	// For local runtime with title generator, use it directly
	if a.titleGen != nil {
		a.titleGenerating.Store(true)

		// Collect user messages for title generation
		var userMessages []string
		for _, msg := range a.session.GetAllMessages() {
			if msg.Message.Role == chat.MessageRoleUser {
				userMessages = append(userMessages, msg.Message.Content)
			}
		}

		go a.generateTitle(ctx, userMessages)
		return nil
	}

	// For remote runtime, title regeneration is not yet supported
	// (the server would need to implement this)
	slog.DebugContext(ctx, "Title regeneration not available for remote runtime", "session_id", a.session.ID)
	return errors.New("title regeneration not available")
}
