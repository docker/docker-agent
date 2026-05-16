package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
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
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/shellpath"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/tools"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type App struct {
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
	titleGenerating        atomic.Bool                 // True when title generation is in progress
	titleGen               *sessiontitle.Generator     // Title generator for local runtime (nil for remote)
	snapshotController     builtins.SnapshotController // Drives /undo, /snapshots, /reset; nil for runtimes that don't capture snapshots

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

func New(ctx context.Context, rt runtime.Runtime, sess *session.Session, opts ...Opt) *App {
	app := &App{
		runtime:          rt,
		session:          sess,
		events:           make(chan tea.Msg, 128),
		throttleDuration: 50 * time.Millisecond, // Throttle rapid events
	}

	for _, opt := range opts {
		opt(app)
	}

	// Emit startup info (agent, team, tools) through the events channel.
	// This runs in the background so the TUI can start immediately while
	// slow operations (like MCP tool loading) complete asynchronously.
	go func() {
		startupEvents := make(chan runtime.Event, 10)
		go func() {
			defer close(startupEvents)
			rt.EmitStartupInfo(ctx, sess, runtime.NewChannelSink(startupEvents))
		}()
		for event := range startupEvents {
			select {
			case app.events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Subscribe to tool list changes so the sidebar updates immediately
	// when an MCP server adds or removes tools (outside of a RunStream).
	rt.OnToolsChanged(func(event runtime.Event) {
		select {
		case app.events <- event:
		case <-ctx.Done():
		}
	})

	return app
}

func (a *App) SendFirstMessage() tea.Cmd {
	if a.firstMessage == nil {
		return nil
	}

	cmds := []tea.Cmd{
		func() tea.Msg {
			// Use the shared PrepareUserMessage function for consistent attachment handling
			userMsg, attachedPath := cli.PrepareUserMessage(context.Background(), a.runtime, *a.firstMessage, a.firstMessageAttach)
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

// CurrentAgentToolsetStatuses returns lifecycle status for each toolset of
// the active agent.
func (a *App) CurrentAgentToolsetStatuses() []tools.ToolsetStatus {
	return a.runtime.CurrentAgentToolsetStatuses()
}

// RestartToolset triggers a supervisor-driven restart of the named toolset.
func (a *App) RestartToolset(ctx context.Context, name string) error {
	return a.runtime.RestartToolset(ctx, name)
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

		content, err := st.ReadSkillContent(ctx, skill.Name)
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
func (a *App) CurrentAgentModel() string {
	if a.currentAgentModel != "" {
		return a.currentAgentModel
	}
	// Fallback to session overrides
	if a.session != nil && a.session.AgentModelOverrides != nil {
		agentName := a.runtime.CurrentAgentName()
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

// EmitStartupInfo emits initial agent, team, and toolset information to the provided channel
func (a *App) EmitStartupInfo(ctx context.Context, events chan runtime.Event) {
	a.runtime.EmitStartupInfo(ctx, a.session, runtime.NewChannelSink(events))
}

// Run one agent loop
func (a *App) Run(ctx context.Context, cancel context.CancelFunc, message string, attachments []messages.Attachment) {
	a.cancel = cancel

	// If this is the first message and no title exists, start local title generation
	if a.session.Title == "" && a.titleGen != nil {
		a.titleGenerating.Store(true)
		go a.generateTitle(ctx, []string{message})
	}

	go func() { //nolint:gosec // background processing intentionally continues after request ctx ends; uses context.Background() only to forward StreamStoppedEvent
		if len(attachments) > 0 {
			// Build a single text string with the user's message and inlined text files.
			// Keeping everything in one text block ensures the model sees file content
			// together with the message, rather than as separate content blocks.
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
				default:
					slog.DebugContext(ctx, "skipping attachment with no file path or content", "name", att.Name)
				}
			}

			multiContent := []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: textBuilder.String()},
			}
			multiContent = append(multiContent, binaryParts...)

			a.session.AddMessage(session.UserMessage(message, multiContent...))
		} else {
			a.session.AddMessage(session.UserMessage(message))
		}
		for event := range a.runtime.RunStream(ctx, a.session) {
			// If context is cancelled, continue draining but don't forward events
			// — except StreamStoppedEvent, which must always propagate so the
			// supervisor can mark the session as no longer running.
			if ctx.Err() != nil {
				if _, ok := event.(*runtime.StreamStoppedEvent); ok {
					a.sendEvent(context.Background(), event)
				}
				continue
			}

			// Clear titleGenerating flag when title is generated (from server for remote runtime)
			if _, ok := event.(*runtime.SessionTitleEvent); ok {
				a.titleGenerating.Store(false)
			}

			a.sendEvent(ctx, event)
		}
	}()
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

// processInlineAttachment handles content that is already in memory (e.g. pasted
// text). The content is appended to textBuilder wrapped in an XML tag for context.
func (a *App) processInlineAttachment(att messages.Attachment, textBuilder *strings.Builder) {
	textBuilder.WriteString("\n\n")
	fmt.Fprintf(textBuilder, "<attached_file path=%q>\n%s\n</attached_file>", att.Name, att.Content)
}

// RunWithMessage runs the agent loop with a pre-constructed message.
// This is used for special cases like image attachments.
func (a *App) RunWithMessage(ctx context.Context, cancel context.CancelFunc, msg *session.Message) {
	a.cancel = cancel

	// If this is the first message and no title exists, start local title generation
	if a.session.Title == "" && a.titleGen != nil {
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

	go func() { //nolint:gosec // background processing intentionally continues after request ctx ends; uses context.Background() only to forward StreamStoppedEvent
		a.session.AddMessage(msg)
		for event := range a.runtime.RunStream(ctx, a.session) {
			// If context is cancelled, continue draining but don't forward events
			// — except StreamStoppedEvent, which must always propagate so the
			// supervisor can mark the session as no longer running.
			if ctx.Err() != nil {
				if _, ok := event.(*runtime.StreamStoppedEvent); ok {
					a.sendEvent(context.Background(), event)
				}
				continue
			}

			// Clear titleGenerating flag when title is generated (from server for remote runtime)
			if _, ok := event.(*runtime.SessionTitleEvent); ok {
				a.titleGenerating.Store(false)
			}

			a.sendEvent(ctx, event)
		}
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
func (a *App) startFanOut() {
	throttled := a.throttleEvents(context.Background(), a.events)
	go func() {
		for msg := range throttled {
			a.subsMu.Lock()
			subs := slices.Clone(a.subs)
			a.subsMu.Unlock()
			for _, ch := range subs {
				select {
				case ch <- msg:
				default:
					slog.Warn("app: subscriber buffer full, dropping event")
				}
			}
		}
	}()
}

// Resume resumes the runtime with the given confirmation request
func (a *App) Resume(req runtime.ResumeRequest) {
	a.runtime.Resume(context.Background(), req)
}

// TogglePause toggles whether the runtime loop is paused at iteration
// boundaries. The second return value is false if the underlying runtime
// doesn't support pausing (e.g. remote runtimes), in which case the first
// return value is meaningless.
func (a *App) TogglePause() (paused, supported bool) {
	p, err := a.runtime.TogglePause(context.Background())
	if errors.Is(err, runtime.ErrUnsupported) {
		return false, false
	}
	if err != nil {
		slog.Error("Failed to toggle pause", "error", err)
		return false, false
	}
	return p, true
}

// ResumeElicitation resumes an elicitation request with the given action and content
func (a *App) ResumeElicitation(ctx context.Context, action tools.ElicitationAction, content map[string]any) error {
	return a.runtime.ResumeElicitation(ctx, action, content)
}

func (a *App) NewSession() {
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	// Preserve user-controlled session flags
	// so they don't reset to default on /new
	var opts []session.Opt
	if a.session != nil {
		opts = append(opts,
			session.WithToolsApproved(a.session.ToolsApproved),
			session.WithHideToolResults(a.session.HideToolResults),
			session.WithWorkingDir(a.session.WorkingDir),
		)
	}
	a.session = session.New(opts...)
	// Clear first message so it won't be re-sent on re-init
	a.firstMessage = nil
	a.firstMessageAttach = ""

	// Re-emit startup info so the sidebar shows agent/tools info in the new session
	a.reEmitStartupInfo(context.Background())
}

// reEmitStartupInfo resets and re-emits startup info (agent, team, tools)
// through the events channel so the sidebar updates.
func (a *App) reEmitStartupInfo(ctx context.Context) {
	a.runtime.ResetStartupInfo()
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
			default:
			}
		}
	}()
}

func (a *App) Session() *session.Session {
	return a.session
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
	return a.runtime.SetCurrentAgent(agentName)
}

// SetCurrentAgentModel sets the model for the current agent and persists
// the override in the session. Returns an error if model switching is not
// supported by the runtime (e.g., remote runtimes).
// Pass an empty modelRef to clear the override and use the agent's default model.
func (a *App) SetCurrentAgentModel(ctx context.Context, modelRef string) error {
	agentName := a.runtime.CurrentAgentName()

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

// AvailableModels returns the list of models available for selection.
// Returns nil if model switching is not supported.
func (a *App) AvailableModels(ctx context.Context) []runtime.ModelChoice {
	if !a.runtime.SupportsModelSwitching() {
		return nil
	}
	models := a.runtime.AvailableModels(ctx)

	// Determine the currently active model for this agent
	agentName := a.runtime.CurrentAgentName()
	currentModelRef := ""
	if a.session != nil && a.session.AgentModelOverrides != nil {
		currentModelRef = a.session.AgentModelOverrides[agentName]
	}

	// Build a set of model refs already in the list
	existingRefs := make(map[string]bool)
	for _, m := range models {
		existingRefs[m.Ref] = true
	}

	// Check if current model is in the list and mark it
	currentFound := currentModelRef == ""
	for i := range models {
		if currentModelRef != "" {
			// An override is set - mark the override as current
			if models[i].Ref == currentModelRef {
				models[i].IsCurrent = true
				currentFound = true
			}
		} else {
			// No override - the default model is current
			models[i].IsCurrent = models[i].IsDefault
		}
	}

	// Add custom models from the session that aren't already in the list
	if a.session != nil {
		for _, customRef := range a.session.CustomModelsUsed {
			if existingRefs[customRef] {
				continue // Already in the list
			}
			existingRefs[customRef] = true

			providerName, modelName, _ := strings.Cut(customRef, "/")
			isCurrent := customRef == currentModelRef
			if isCurrent {
				currentFound = true
			}
			models = append(models, runtime.ModelChoice{
				Name:      customRef,
				Ref:       customRef,
				Provider:  providerName,
				Model:     modelName,
				IsDefault: false,
				IsCurrent: isCurrent,
				IsCustom:  true,
			})
		}
	}

	// If current model is a custom model not in the list, add it
	if !currentFound && strings.Contains(currentModelRef, "/") {
		providerName, modelName, _ := strings.Cut(currentModelRef, "/")
		models = append(models, runtime.ModelChoice{
			Name:      currentModelRef,
			Ref:       currentModelRef,
			Provider:  providerName,
			Model:     modelName,
			IsDefault: false,
			IsCurrent: true,
			IsCustom:  true,
		})
	}

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

func (a *App) CompactSession(ctx context.Context, additionalPrompt string) {
	sess := a.session
	if sess == nil {
		return
	}

	go func() {
		events := make(chan runtime.Event, 100)
		go func() {
			defer close(events)
			a.runtime.Summarize(ctx, sess, additionalPrompt, runtime.NewChannelSink(events))
		}()
		for event := range events {
			if ctx.Err() != nil {
				return
			}
			a.sendEvent(ctx, event)
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
