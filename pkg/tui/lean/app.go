package lean

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	appcore "github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	tuiMessages "github.com/docker/docker-agent/pkg/tui/messages"
)

type promptKind string

const (
	promptToolConfirm promptKind = "tool-confirm"
	promptMaxIters    promptKind = "max-iters"
	promptElicitation promptKind = "elicitation"
)

type promptState struct {
	kind        promptKind
	tool        *runtime.ToolCallConfirmationEvent
	maxIters    int
	elicitation *runtime.ElicitationRequestEvent
}

type App struct {
	application *appcore.App
	options     Options
	descriptor  SessionDescriptor

	ui          *TUI
	transcript  *Container
	pendingArea *Container
	editor      *Editor
	header      *HeaderBlock
	footer      *FooterBlock
	md          *markdownRenderer

	usage      UsageTotals
	connection ConnectionStatus
	streaming  bool
	exiting    bool

	toolsExpanded   bool
	thinkingHidden  bool
	hideToolResults bool

	cancel    context.CancelFunc
	runCancel context.CancelFunc

	currentAssistant     *AssistantBlock
	currentThinking      *ThinkingBlock
	currentAssistantText string
	currentThinkingText  string
	currentInsertAnchor  Component
	toolBlocks           map[string]*ToolBlock
	thinkingBlocks       []*ThinkingBlock
	pendingPrompt        *promptState
	hasAssistantContent  bool
	queuedMessages       []string

	shutdownCh chan struct{}
}

func NewApp(application *appcore.App, descriptor SessionDescriptor, opts Options) *App {
	terminal := NewProcessTerminal()
	ui := NewTUI(terminal)
	md := newMarkdownRenderer()
	app := &App{
		application:     application,
		options:         opts,
		descriptor:      descriptor,
		ui:              ui,
		transcript:      &Container{},
		pendingArea:     &Container{},
		editor:          NewEditor(),
		header:          &HeaderBlock{Descriptor: descriptor, Connection: ConnectionConnected},
		footer:          &FooterBlock{Descriptor: descriptor},
		md:              md,
		connection:      ConnectionConnected,
		cancel:          func() {},
		toolBlocks:      make(map[string]*ToolBlock),
		queuedMessages:  append([]string(nil), opts.QueuedMessages...),
		hideToolResults: application.Session() != nil && application.Session().HideToolResults,
		shutdownCh:      make(chan struct{}),
	}
	app.editor.OnSubmit(func(text string) { go app.handleSubmit(text, nil) })
	app.buildLayout()
	app.installGlobalShortcuts()
	return app
}

func (a *App) Run() error {
	history := a.application.Session().GetAllMessages()
	a.renderHistory(history)
	if len(history) == 0 {
		a.appendTranscriptBlock(&BannerBlock{})
	}
	a.syncUsageFromSession()
	a.refreshChrome()
	if err := a.ui.Start(); err != nil {
		return err
	}
	a.ui.SetFocus(a.editor)
	subscribeCtx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	go a.application.SubscribeWith(subscribeCtx, func(msg tea.Msg) {
		if ev, ok := msg.(runtime.Event); ok {
			a.handleEvent(ev)
		}
	})
	if a.options.FirstMessage != "" {
		var attachments []tuiMessages.Attachment
		if a.options.FirstMessageAttachment != "" {
			attachments = append(attachments, tuiMessages.Attachment{
				Name:     filepath.Base(a.options.FirstMessageAttachment),
				FilePath: a.options.FirstMessageAttachment,
			})
		}
		go a.handleSubmit(a.options.FirstMessage, attachments)
	}
	<-a.shutdownCh
	return nil
}

func (a *App) buildLayout() {
	a.ui.AddChild(a.header)
	a.ui.AddChild(Spacer{Height: 1})
	a.ui.AddChild(a.transcript)
	a.ui.AddChild(a.pendingArea)
	a.ui.AddChild(a.editor)
	a.ui.AddChild(a.footer)
}

func (a *App) installGlobalShortcuts() {
	a.ui.AddInputListener(func(data string) bool {
		key := ParseKey(data)
		if a.pendingPrompt != nil {
			if a.handlePromptKey(key) {
				return true
			}
		}
		switch key {
		case "ctrl+c":
			a.shutdown()
			return true
		case "escape":
			switch {
			case a.pendingPrompt != nil:
				a.cancelPrompt()
			case a.streaming:
				a.cancelActiveRun()
			default:
				a.shutdown()
			}
			return true
		case "ctrl+t":
			a.toggleToolsExpanded()
			return true
		case "ctrl+g":
			a.toggleThinkingHidden()
			return true
		default:
			return false
		}
	})
}

func (a *App) refreshChrome() {
	a.header.Descriptor = a.descriptor
	a.header.Connection = a.connection
	a.header.ToolsExpanded = a.toolsExpanded
	a.header.ThinkingHidden = a.thinkingHidden
	a.header.PromptActive = a.pendingPrompt != nil
	a.footer.Descriptor = a.descriptor
	a.footer.Usage = a.usage
	a.footer.Streaming = a.streaming
	if a.pendingPrompt != nil {
		a.pendingArea.SetChildren(a.pendingPromptBlock())
	} else {
		a.pendingArea.SetChildren()
	}
	switch {
	case a.streaming:
		a.editor.SetBorderColor(yellowStyle.Render)
	case a.pendingPrompt != nil:
		a.editor.SetBorderColor(magentaStyle.Render)
	default:
		a.editor.SetBorderColor(blueStyle.Render)
	}
	a.editor.SetDisableSubmit(a.streaming || a.pendingPrompt != nil)
	a.ui.RequestRender()
}

func (a *App) setStreaming(streaming bool) {
	a.streaming = streaming
	a.refreshChrome()
}

func (a *App) toggleToolsExpanded() {
	a.toolsExpanded = !a.toolsExpanded
	for _, tool := range a.toolBlocks {
		tool.Expanded = a.toolsExpanded
	}
	a.refreshChrome()
}

func (a *App) toggleThinkingHidden() {
	a.thinkingHidden = !a.thinkingHidden
	for _, block := range a.thinkingBlocks {
		block.Hidden = a.thinkingHidden
	}
	a.refreshChrome()
}

func (a *App) handleSubmit(text string, attachments []tuiMessages.Attachment) {
	input := strings.TrimSpace(text)
	if input == "" || a.streaming || a.pendingPrompt != nil {
		return
	}
	a.appendTranscriptBlock(&UserBlock{Content: text})
	a.editor.Reset()
	a.setStreaming(true)
	runCtx, cancel := context.WithCancel(context.Background())
	a.runCancel = cancel
	a.application.Run(runCtx, cancel, text, attachments)
}

func (a *App) cancelActiveRun() {
	if !a.streaming {
		return
	}
	if a.runCancel != nil {
		a.runCancel()
		a.runCancel = nil
	}
	a.appendTranscriptBlock(&NoticeBlock{Message: "Cancellation requested…", Kind: "info"})
	a.ui.RequestRender()
}

func (a *App) shutdown() {
	if a.exiting {
		return
	}
	a.exiting = true
	if a.runCancel != nil {
		a.runCancel()
		a.runCancel = nil
	}
	a.cancel()
	a.ui.Stop()
	close(a.shutdownCh)
}

func (a *App) handleEvent(event runtime.Event) {
	if event == nil {
		return
	}
	switch ev := event.(type) {
	case *runtime.StreamStartedEvent:
		a.setStreaming(true)
	case *runtime.StreamStoppedEvent:
		a.finalizeStreamingState()
		a.runCancel = nil
		a.setStreaming(false)
		if a.options.ExitAfterFirstResponse && a.hasAssistantContent {
			a.shutdown()
			return
		}
		a.processNextQueuedMessage()
	case *runtime.AgentChoiceReasoningEvent:
		a.handleThinkingDelta(ev.Content)
	case *runtime.AgentChoiceEvent:
		a.handleAssistantDelta(ev.Content)
	case *runtime.PartialToolCallEvent:
		a.handleToolCallDelta(ev.ToolCall.ID, ev.ToolCall.Function.Name, ev.ToolCall.Function.Arguments)
	case *runtime.ToolCallConfirmationEvent:
		a.handleToolCallConfirmation(ev)
	case *runtime.ToolCallEvent:
		a.handleToolCallStart(ev.ToolCall.ID, ev.ToolCall.Function.Name, ev.ToolCall.Function.Arguments)
	case *runtime.ToolCallResponseEvent:
		a.handleToolCallResult(ev.ToolCallID, ev.ToolDefinition.Name, ev.Response, ev.Result != nil && ev.Result.IsError)
	case *runtime.TokenUsageEvent:
		a.applyUsage(ev.Usage)
	case *runtime.ErrorEvent:
		a.appendTranscriptBlock(&NoticeBlock{Message: ev.Error, Kind: "error"})
	case *runtime.WarningEvent:
		a.appendTranscriptBlock(&NoticeBlock{Message: ev.Message, Kind: "info"})
	case *runtime.MaxIterationsReachedEvent:
		a.pendingPrompt = &promptState{kind: promptMaxIters, maxIters: ev.MaxIterations}
		a.setStreaming(false)
	case *runtime.ElicitationRequestEvent:
		a.pendingPrompt = &promptState{kind: promptElicitation, elicitation: ev}
		a.setStreaming(false)
	case *runtime.AgentInfoEvent:
		a.descriptor.AgentName = orDefault(ev.AgentName, a.descriptor.AgentName)
		a.descriptor.Model = orDefault(ev.Model, a.descriptor.Model)
		if len(a.application.Session().GetAllMessages()) == 0 && ev.WelcomeMessage != "" {
			a.appendTranscriptBlock(&NoticeBlock{Message: ev.WelcomeMessage, Kind: "info"})
		}
	case *runtime.SessionTitleEvent:
		// no-op for now; lean TUI keeps the header compact.
	case *runtime.ModelFallbackEvent:
		a.appendTranscriptBlock(&NoticeBlock{Message: fmt.Sprintf("Model fallback: %s → %s (%s)", ev.FailedModel, ev.FallbackModel, ev.Reason), Kind: "info"})
	case *runtime.HookBlockedEvent:
		a.handleToolCallResult(ev.ToolCall.ID, ev.ToolDefinition.Name, ev.Message, true)
	case *runtime.SessionCompactionEvent:
		a.appendTranscriptBlock(&NoticeBlock{Message: fmt.Sprintf("Session compaction %s.", ev.Status), Kind: "info"})
	case *runtime.AgentSwitchingEvent:
		if ev.Switching {
			a.appendTranscriptBlock(&NoticeBlock{Message: fmt.Sprintf("Switching agent: %s → %s", ev.FromAgent, ev.ToAgent), Kind: "info"})
		}
	case *runtime.UserMessageEvent:
		if !a.streaming && strings.TrimSpace(ev.Message) != "" {
			a.appendTranscriptBlock(&NoticeBlock{Message: ev.Message, Kind: "sub-agent"})
		}
	}
	a.refreshChrome()
}

func (a *App) applyUsage(usage *runtime.Usage) {
	if usage == nil {
		return
	}
	a.usage.Prompt = usage.InputTokens
	a.usage.Completion = usage.OutputTokens
	a.usage.Total = usage.ContextLength
	a.usage.Cost = usage.Cost
	if usage.LastMessage != nil {
		a.usage.CacheRead = usage.LastMessage.CachedInputTokens
		a.usage.CacheCreation = usage.LastMessage.CacheWriteTokens
		if usage.LastMessage.Model != "" {
			a.descriptor.Model = usage.LastMessage.Model
		}
	}
}

func (a *App) syncUsageFromSession() {
	sess := a.application.Session()
	if sess == nil {
		return
	}
	a.usage.Prompt = sess.InputTokens
	a.usage.Completion = sess.OutputTokens
	a.usage.Total = sess.InputTokens + sess.OutputTokens
	a.usage.Cost = sess.OwnCost()
}

func (a *App) handleThinkingDelta(delta string) {
	if delta == "" {
		return
	}
	a.currentThinkingText += delta
	if a.currentThinking == nil {
		block := &ThinkingBlock{Content: a.currentThinkingText, Hidden: a.thinkingHidden, MD: a.md}
		a.thinkingBlocks = append(a.thinkingBlocks, block)
		a.appendTranscriptBlock(block)
		a.currentThinking = block
		return
	}
	a.currentThinking.Content = a.currentThinkingText
}

func (a *App) handleAssistantDelta(delta string) {
	if delta == "" {
		return
	}
	a.hasAssistantContent = true
	a.currentAssistantText += delta
	if a.currentAssistant == nil {
		block := &AssistantBlock{Content: a.currentAssistantText, MD: a.md}
		a.appendTranscriptBlock(block)
		a.currentAssistant = block
		return
	}
	a.currentAssistant.Content = a.currentAssistantText
}

func (a *App) getOrCreateToolBlock(toolCallID, name, initialArgs string) *ToolBlock {
	if strings.TrimSpace(toolCallID) == "" {
		return nil
	}
	if block, ok := a.toolBlocks[toolCallID]; ok {
		return block
	}
	block := &ToolBlock{ID: toolCallID, ToolName: name, Args: initialArgs, Status: ToolPending, Expanded: a.toolsExpanded, HideToolResults: a.hideToolResults}
	a.toolBlocks[toolCallID] = block
	a.insertTranscriptBlockAfter(a.currentInsertAnchor, block)
	return block
}

func (a *App) handleToolCallDelta(toolCallID, name, argsDelta string) {
	block := a.getOrCreateToolBlock(toolCallID, name, "")
	if block == nil {
		return
	}
	block.ToolName = name
	block.Args += argsDelta
	a.currentInsertAnchor = block
}

func (a *App) handleToolCallConfirmation(ev *runtime.ToolCallConfirmationEvent) {
	block := a.getOrCreateToolBlock(ev.ToolCall.ID, ev.ToolCall.Function.Name, ev.ToolCall.Function.Arguments)
	if block != nil {
		block.ToolName = ev.ToolCall.Function.Name
		block.Status = ToolConfirmation
	}
	a.pendingPrompt = &promptState{kind: promptToolConfirm, tool: ev}
	a.currentInsertAnchor = block
}

func (a *App) handleToolCallStart(toolCallID, name, fullArgs string) {
	block := a.getOrCreateToolBlock(toolCallID, name, fullArgs)
	if block == nil {
		return
	}
	block.ToolName = name
	if len(fullArgs) >= len(block.Args) {
		block.Args = fullArgs
	}
	block.Status = ToolRunning
	a.pendingPrompt = nil
	a.currentAssistant = nil
	a.currentThinking = nil
	a.currentAssistantText = ""
	a.currentThinkingText = ""
	a.currentInsertAnchor = block
}

func (a *App) handleToolCallResult(toolCallID, name, content string, isError bool) {
	block := a.getOrCreateToolBlock(toolCallID, name, "")
	if block == nil {
		return
	}
	block.ToolName = name
	block.Result = content
	if isError {
		block.Status = ToolError
	} else {
		block.Status = ToolCompleted
	}
	a.currentInsertAnchor = block
}

func (a *App) finalizeStreamingState() {
	a.currentAssistant = nil
	a.currentThinking = nil
	a.currentAssistantText = ""
	a.currentThinkingText = ""
	a.currentInsertAnchor = nil
	a.pendingPrompt = nil
}

func (a *App) renderHistory(history []session.Message) {
	toolResults := map[string]session.Message{}
	for _, message := range history {
		if message.Message.Role == chat.MessageRoleTool && message.Message.ToolCallID != "" {
			toolResults[message.Message.ToolCallID] = message
		}
	}
	for _, message := range history {
		if message.Implicit {
			continue
		}
		switch message.Message.Role {
		case chat.MessageRoleUser:
			a.appendTranscriptBlock(&UserBlock{Content: message.Message.Content})
		case chat.MessageRoleAssistant:
			if message.Message.ReasoningContent != "" {
				block := &ThinkingBlock{Content: message.Message.ReasoningContent, Hidden: a.thinkingHidden, MD: a.md}
				a.thinkingBlocks = append(a.thinkingBlocks, block)
				a.appendTranscriptBlock(block)
			}
			if message.Message.Content != "" {
				a.appendTranscriptBlock(&AssistantBlock{Content: message.Message.Content, MD: a.md})
			}
			for _, toolCall := range message.Message.ToolCalls {
				tool := &ToolBlock{ID: toolCall.ID, ToolName: toolCall.Function.Name, Expanded: a.toolsExpanded, Args: toolCall.Function.Arguments, HideToolResults: a.hideToolResults}
				if result, ok := toolResults[toolCall.ID]; ok {
					tool.Result = result.Message.Content
					if result.Message.IsError {
						tool.Status = ToolError
					} else {
						tool.Status = ToolCompleted
					}
				} else {
					tool.Status = ToolRunning
				}
				a.toolBlocks[tool.ID] = tool
				a.appendTranscriptBlock(tool)
			}
		}
	}
}

func (a *App) appendTranscriptBlock(block Component) {
	a.transcript.AddChild(block)
	a.currentInsertAnchor = block
}

func (a *App) insertTranscriptBlockAfter(anchor, block Component) {
	if anchor == nil {
		a.appendTranscriptBlock(block)
		return
	}
	for i, child := range a.transcript.Children {
		if child != anchor {
			continue
		}
		children := append([]Component{}, a.transcript.Children[:i+1]...)
		children = append(children, block)
		children = append(children, a.transcript.Children[i+1:]...)
		a.transcript.Children = children
		a.currentInsertAnchor = block
		return
	}
	a.appendTranscriptBlock(block)
}

func (a *App) processNextQueuedMessage() {
	if a.streaming || len(a.queuedMessages) == 0 || a.pendingPrompt != nil {
		return
	}
	next := a.queuedMessages[0]
	a.queuedMessages = a.queuedMessages[1:]
	go a.handleSubmit(next, nil)
}

func (a *App) pendingPromptBlock() Component {
	if a.pendingPrompt == nil {
		return nil
	}
	switch a.pendingPrompt.kind {
	case promptToolConfirm:
		toolName := a.pendingPrompt.tool.ToolCall.Function.Name
		return &PromptBlock{
			Title:   "tool approval",
			Body:    fmt.Sprintf("Allow %s to run?", toolName),
			Actions: "Y approve once · N reject · T always allow this tool · A allow all for session · Esc reject",
		}
	case promptMaxIters:
		return &PromptBlock{
			Title:   "max iterations",
			Body:    fmt.Sprintf("The agent reached the max iteration limit (%d).", a.pendingPrompt.maxIters),
			Actions: "Y continue · N stop",
		}
	case promptElicitation:
		body := strings.TrimSpace(a.pendingPrompt.elicitation.Message)
		if a.pendingPrompt.elicitation.URL != "" {
			body += "\n\n" + a.pendingPrompt.elicitation.URL
		}
		actions := "Y accept · N decline · C cancel · Esc cancel"
		if a.pendingPrompt.elicitation.Mode != "url" && !isOAuthPrompt(a.pendingPrompt.elicitation) {
			actions = "N decline · C cancel · Esc cancel · form input requires full TUI"
		}
		return &PromptBlock{Title: "elicitation", Body: body, Actions: actions}
	default:
		return nil
	}
}

func isOAuthPrompt(ev *runtime.ElicitationRequestEvent) bool {
	if ev == nil || ev.Meta == nil {
		return false
	}
	kind, _ := ev.Meta["cagent/type"].(string)
	return kind == "oauth_flow"
}

func (a *App) handlePromptKey(key string) bool {
	if a.pendingPrompt == nil {
		return false
	}
	switch a.pendingPrompt.kind {
	case promptToolConfirm:
		switch strings.ToLower(key) {
		case "y":
			a.application.Resume(runtime.ResumeApprove())
		case "n", "escape":
			a.application.Resume(runtime.ResumeReject("rejected in lean TUI"))
		case "t":
			pattern := a.pendingPrompt.tool.ToolCall.Function.Name
			a.application.Resume(runtime.ResumeApproveTool(pattern))
		case "a":
			a.application.Resume(runtime.ResumeApproveSession())
		default:
			return false
		}
		a.pendingPrompt = nil
		a.refreshChrome()
		return true
	case promptMaxIters:
		switch strings.ToLower(key) {
		case "y":
			a.application.Resume(runtime.ResumeApprove())
		case "n", "escape":
			a.application.Resume(runtime.ResumeReject("stopped after reaching max iterations"))
		default:
			return false
		}
		a.pendingPrompt = nil
		a.refreshChrome()
		return true
	case promptElicitation:
		var action tools.ElicitationAction
		switch strings.ToLower(key) {
		case "y":
			if a.pendingPrompt.elicitation.Mode != "url" && !isOAuthPrompt(a.pendingPrompt.elicitation) {
				return false
			}
			action = tools.ElicitationActionAccept
		case "n":
			action = tools.ElicitationActionDecline
		case "c", "escape":
			action = tools.ElicitationActionCancel
		default:
			return false
		}
		_ = a.application.ResumeElicitation(context.Background(), action, nil)
		a.pendingPrompt = nil
		a.refreshChrome()
		return true
	default:
		return false
	}
}

func (a *App) cancelPrompt() {
	if a.pendingPrompt == nil {
		return
	}
	_ = a.handlePromptKey("escape")
}
