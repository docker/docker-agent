package leantui

import (
	"context"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/sound"
	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// handleEvent applies a single runtime event emitted by the App to the model,
// updating the conversation, tool state, status footer, or busy state.
func (m *model) handleEvent(ctx context.Context, ev any) {
	if routed, ok := ev.(leanEvent); ok {
		if (routed.valid != nil && !routed.valid()) || routed.generation != m.eventGeneration.Load() {
			return
		}
		ev = routed.inner
	}
	if m.app != nil && !m.app.IsCurrentOperation(ev) {
		return
	}
	if m.programStatus != nil {
		m.statusSession.Apply(m.contentSession(""), ev)
	}
	switch e := ev.(type) {
	case fileCompletionsLoaded:
		m.screen.Autocomplete.SetFiles(e)
		m.screen.Autocomplete.Sync(m.screen.Editor.Text())
	case msgtypes.SendMsg:
		if e.BypassQueue {
			m.submit(ctx, e.Content, submitOptions{busyMode: busySubmitSteer})
		} else {
			m.submitFollowUp(ctx, e.Content)
		}
	case *runtime.StreamStartedEvent:
		m.contentIdentity.Finish(m.contentSession(e.SessionID))
		if m.streamDepth == 0 {
			m.streamStartTime = time.Now()
		}
		m.streamDepth++
		m.busy = true
		m.trackStreamStarted(e.SessionID)
	case *runtime.UserMessageEvent:
		m.handleUserMessageEvent(e)
	case *runtime.SessionRecoveredEvent:
		if e.SessionID != "" && e.SessionID != m.contentSession("") {
			return
		}
		m.screen.Transcript.FlushPending()
		m.screen.Transcript.FinalizeTools(tuitypes.ToolStatusError, m.sessionState)
		m.streamDepth = 0
		m.usage.RecoverIdle(m.contentSession(e.SessionID))
		m.applyUsageSnapshot()
		m.busy = false
		m.runCancel = nil
		m.cancelMarkerPending = false
		m.screen.Confirm = nil
		m.status.Compacting = false
		m.contentIdentity.Finish(m.contentSession(e.SessionID))
	case *runtime.StreamStoppedEvent:
		m.contentIdentity.Finish(m.contentSession(e.SessionID))
		m.trackStreamStopped()
		m.streamDepth = max(0, m.streamDepth-1)
		if m.streamDepth > 0 {
			return
		}
		m.statusSession.FinishSetup(m.contentSession(""), e.Reason)
		m.notifyStreamStopped(ctx, e.Reason)
		m.handleStreamStopped(ctx)
	case *runtime.AgentChoiceReasoningResetEvent:
		m.screen.Transcript.ResetReasoningContent(m.contentIdentity.Resolve(m.contentSession(e.SessionID), e.MessageID))
	case *runtime.AgentChoiceReasoningEvent:
		m.screen.Transcript.AppendReasoningContent(m.contentIdentity.Resolve(m.contentSession(e.SessionID), e.MessageID), e.Content)
	case *runtime.AgentChoiceEvent:
		m.screen.Transcript.AppendAssistantContent(m.contentIdentity.Resolve(m.contentSession(e.SessionID), e.MessageID), e.Content)
	case *runtime.MessageAddedEvent:
		if e.Message == nil || e.Message.Implicit || e.Message.Message.Role != chat.MessageRoleAssistant {
			return
		}
		sessionID := m.contentSession(e.SessionID)
		identity := m.contentIdentity.Resolve(sessionID, e.Message.Message.MessageID)
		m.screen.Transcript.ReconcileAssistantContent(identity, chat.VisibleAssistantContent(e.Message.Message.Content))
		m.contentIdentity.Finish(sessionID)
	case *runtime.ToolCallsRetractedEvent:
		for _, id := range e.ToolCallIDs {
			m.screen.Transcript.RemoveTool(id)
		}
	case *runtime.PartialToolCallEvent:
		m.screen.Transcript.FlushPending()
		toolDef := tools.Tool{Name: e.ToolCall.Function.Name}
		if e.ToolDefinition != nil {
			toolDef = *e.ToolDefinition
		}
		m.screen.Transcript.UpsertTool(e.GetAgentName(), e.ToolCall, toolDef, tuitypes.ToolStatusPending)
	case *runtime.ToolCallEvent:
		m.screen.Transcript.FlushPending()
		m.screen.Transcript.UpsertTool(e.GetAgentName(), e.ToolCall, e.ToolDefinition, tuitypes.ToolStatusRunning)
	case *runtime.ToolCallOutputEvent:
		if tv := m.screen.Transcript.Tool(e.ToolCallID); tv != nil && tv.Message() != nil {
			tv.Message().AppendToolOutput(e.Output)
			if tv.Message().ToolStatus == tuitypes.ToolStatusPending {
				tv.Message().SetToolStatus(tuitypes.ToolStatusRunning)
			}
		}
	case *runtime.ToolCallResponseEvent:
		var images []ui.InlineImage
		if m.renderImages {
			images = tuiimage.FromToolResult(e.Result)
		}
		m.screen.Transcript.FinishTool(e.ToolCallID, ui.ToolResult{Response: e.Response, Result: e.Result, AgentName: e.GetAgentName(), ToolDefinition: e.ToolDefinition, Images: images}, m.sessionState)
	case *runtime.ToolCallConfirmationEvent:
		m.confirmationEvent = e
		m.screen.Transcript.RemoveTool(ui.ToolViewID(e.ToolCall))
		toolDef := ui.EnsureToolDefinition(e.ToolCall, e.ToolDefinition)
		m.screen.Confirm = &ui.ConfirmModel{
			Tool: toolDef.Name,
			View: *ui.NewToolView(e.GetAgentName(), e.ToolCall, toolDef, tuitypes.ToolStatusConfirmation),
		}
	case *runtime.TokenUsageEvent:
		m.setTokenUsage(e.SessionID, e.Usage)
	case *runtime.AgentInfoEvent:
		m.status.Agent = e.AgentName
		if m.sessionState != nil {
			m.sessionState.SetCurrentAgentName(e.AgentName)
		}
		if e.Model != "" {
			m.status.Model = e.Model
		}
		if e.ContextLimit > 0 {
			m.status.ContextLimit = e.ContextLimit
		}
	case *runtime.TeamInfoEvent:
		m.applyTeamInfo(ctx, e)
	case *runtime.SessionCompactionEvent:
		m.handleSessionCompaction(ctx, e)
	case *runtime.ErrorEvent:
		if m.playSound != nil && userconfig.Get().GetSound() {
			m.playSound(ctx, sound.Failure)
		}
		m.screen.Transcript.FlushPending()
		m.addNotice("✗ ", e.Error, ui.StError())
	case *runtime.WarningEvent:
		m.addNotice("⚠ ", e.Message, ui.StWarning())
	case *runtime.ShellOutputEvent:
		if e.CommandID == "" {
			output := e.Output
			m.screen.Transcript.AddBlock(func(w int) []string { return ui.RenderToolOutput(output, w) })
			break
		}

		toolDef := tools.Tool{Name: builtinshell.ToolNameShell}
		if e.Command != "" && m.screen.Transcript.Tool(e.CommandID) == nil {
			m.screen.Transcript.UpsertTool("", shellCommandCall(e.CommandID, e.Command), toolDef, tuitypes.ToolStatusRunning)
		}
		if !e.Done {
			if tv := m.screen.Transcript.Tool(e.CommandID); tv != nil && tv.Message() != nil {
				tv.Message().AppendToolOutput(e.Output)
			}
			break
		}

		response := e.Output
		if e.Error != "" && response == "" {
			response = "Error: " + e.Error
		}
		m.screen.Transcript.FinishTool(e.CommandID, ui.ToolResult{
			Response:       response,
			Result:         &tools.ToolCallResult{Output: response, IsError: e.Error != ""},
			ToolDefinition: toolDef,
		}, m.sessionState)
	case *runtime.AgentSwitchingEvent:
		if e.Switching && e.ToAgent != "" {
			m.addNotice("→ ", "Switching to "+e.ToAgent, ui.StMuted())
		}
	case *runtime.AgentRouteEvent:
		m.addNotice("→ ", "Routing to "+e.ToAgent, ui.StMuted())
	case *runtime.MaxIterationsReachedEvent:
		m.addNotice("⚠ ", "Maximum iterations reached.", ui.StWarning())
	case *runtime.ModelFallbackEvent:
		m.addNotice("⚠ ", "Model "+e.FailedModel+" failed, falling back to "+e.FallbackModel+".", ui.StWarning())
	}
}

func (m *model) handleUserMessageEvent(e *runtime.UserMessageEvent) {
	if m.consumeIgnoredUserEcho(e.Message) {
		return
	}
	if pending, ok := m.consumePendingUser(ui.PendingUserSteer, e.Message); ok {
		m.screen.Transcript.FlushPending()
		m.addUserEcho(pending.Display)
		return
	}
	if pending, ok := m.consumePendingUser(ui.PendingUserFollowUp, e.Message); ok {
		m.screen.Transcript.FlushPending()
		m.addUserEcho(pending.Display)
		return
	}
	m.screen.Transcript.FlushPending()
	m.addUserEcho(e.Message)
}

func (m *model) handleStreamStopped(ctx context.Context) {
	if m.finishBusy(ctx) {
		return
	}

	if m.app != nil && m.app.ShouldExitAfterFirstResponse() {
		m.quit()
	}
}

func (m *model) handleSessionCompaction(ctx context.Context, e *runtime.SessionCompactionEvent) {
	switch e.Status {
	case "started":
		m.busy = true
		m.status.Compacting = true
	case "completed":
		m.status.Compacting = false
		m.finishBusy(ctx)
	}
}

// finishBusy clears the busy state at the end of a run and starts the next
// queued message, if any. It reports whether a queued run was started.
func (m *model) finishBusy(ctx context.Context) bool {
	m.screen.Transcript.FlushPending()
	m.screen.Transcript.FinalizeTools(tuitypes.ToolStatusError, m.sessionState)
	if m.cancelMarkerPending {
		m.screen.Transcript.AddBlock(func(int) []string { return []string{ui.StWarning().Render("⏹ Cancelled")} })
		m.cancelMarkerPending = false
	}
	m.busy = false
	m.runCancel = nil

	if len(m.queue) > 0 {
		next := m.queue[0]
		m.queue[0] = ui.PendingUserMessage{}
		m.queue = m.queue[1:]
		if pending, ok := m.consumePendingUser(ui.PendingUserFollowUp, next.Content); ok {
			next.Display = pending.Display
		}
		m.addUserEcho(next.Display)
		m.ignoreUserEcho(next.Content)
		m.startRun(ctx, next.Content, nil)
		return true
	}
	return false
}

func (m *model) applyTeamInfo(ctx context.Context, e *runtime.TeamInfoEvent) {
	if m.sessionState != nil {
		m.sessionState.SetAvailableAgents(e.AvailableAgents)
		m.sessionState.SetCurrentAgentName(e.CurrentAgent)
	}
	for _, a := range e.AvailableAgents {
		if a.Name != e.CurrentAgent {
			continue
		}
		m.status.Agent = a.Name
		switch {
		case a.Provider != "" && a.Model != "":
			m.status.Model = a.Provider + "/" + a.Model
		case a.Model != "":
			m.status.Model = a.Model
		}
		m.status.Thinking = a.Thinking
	}
	m.refreshCommands(ctx)
}

func (m *model) notifyStreamStopped(ctx context.Context, reason string) {
	if m.playSound == nil || m.streamStartTime.IsZero() {
		return
	}
	defer func() { m.streamStartTime = time.Time{} }()
	switch reason {
	case "", "normal", "continue", "steered":
		settings := userconfig.Get()
		if settings.GetSound() && time.Since(m.streamStartTime) >= time.Duration(settings.GetSoundThreshold())*time.Second {
			m.playSound(ctx, sound.Success)
		}
	}
}

func (m *model) contentSession(sessionID string) string {
	if sessionID == "" && m.app != nil && m.app.Session() != nil {
		return m.app.Session().ID
	}
	return sessionID
}
