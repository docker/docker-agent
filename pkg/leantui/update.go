package leantui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/modelpicker"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func (m *model) handleKey(ctx context.Context, k ui.Key) {
	if m.screen.Confirm != nil {
		m.handleConfirmKey(k)
		return
	}

	switch k.Typ {
	case ui.KeyCtrlC:
		m.handleInterrupt()
	case ui.KeyCtrlD:
		if m.screen.Editor.IsEmpty() {
			m.quit()
		} else {
			m.screen.Editor.DeleteForward()
		}
	case ui.KeyEnter:
		m.handleEnter(ctx)
	case ui.KeyAltEnter:
		m.screen.Editor.InsertNewline()
	case ui.KeyTab:
		m.handleTab()
	case ui.KeyShiftTab:
		m.handleCycleThinkingLevel(ctx)
	case ui.KeyUp:
		if m.screen.Autocomplete.Active {
			m.screen.Autocomplete.MoveUp()
		} else if !m.screen.Editor.Up(m.width) {
			m.screen.Editor.HistoryPrev()
		}
	case ui.KeyDown:
		if m.screen.Autocomplete.Active {
			m.screen.Autocomplete.MoveDown()
		} else if !m.screen.Editor.Down(m.width) {
			m.screen.Editor.HistoryNext()
		}
	case ui.KeyLeft:
		m.screen.Editor.MoveLeft()
	case ui.KeyRight:
		m.screen.Editor.MoveRight()
	case ui.KeyWordLeft:
		m.screen.Editor.MoveWordLeft()
	case ui.KeyWordRight:
		m.screen.Editor.MoveWordRight()
	case ui.KeyHome:
		m.screen.Editor.MoveLineStart()
	case ui.KeyEnd:
		m.screen.Editor.MoveLineEnd()
	case ui.KeyBackspace:
		m.screen.Editor.Backspace()
	case ui.KeyDelete:
		m.screen.Editor.DeleteForward()
	case ui.KeyCtrlU:
		m.screen.Editor.DeleteToLineStart()
	case ui.KeyCtrlK:
		m.screen.Editor.DeleteToLineEnd()
	case ui.KeyCtrlW:
		m.screen.Editor.DeleteWordBack()
	case ui.KeyEsc:
		m.screen.Autocomplete.Dismiss()
	case ui.KeyCtrlL:
		m.clearScreen()
	case ui.KeyRune, ui.KeyPaste:
		m.screen.Editor.Insert(k.Runes)
	}

	m.screen.Autocomplete.Sync(m.screen.Editor.Text())
}

func (m *model) handleInterrupt() {
	switch {
	case m.busy:
		if m.runCancel != nil {
			m.runCancel()
		}
		m.queue = nil
		m.pendingUsers = nil
		m.ignoredUsers = nil
		m.screen.Transcript.AddBlock(func(int) []string { return []string{ui.StWarning().Render("⏹ Cancelled")} })
	case !m.screen.Editor.IsEmpty():
		m.screen.Editor.Reset()
		m.screen.Autocomplete.Dismiss()
	default:
		m.quit()
	}
}

func (m *model) handleEnter(ctx context.Context) {
	if m.screen.Autocomplete.Active {
		if cmd, ok := m.screen.Autocomplete.Current(); ok {
			completion := m.screen.Autocomplete.Completion(cmd)
			m.screen.Autocomplete.Dismiss()
			m.submitEditor(ctx, completion)
			return
		}
	}
	m.submitEditor(ctx, m.screen.Editor.Text())
}

func (m *model) handleTab() {
	if !m.screen.Autocomplete.Active {
		return
	}
	if cmd, ok := m.screen.Autocomplete.Current(); ok {
		m.screen.Editor.SetText(m.screen.Autocomplete.Completion(cmd) + " ")
		m.screen.Autocomplete.Sync(m.screen.Editor.Text())
	}
}

func (m *model) handleCycleThinkingLevel(ctx context.Context) {
	if !m.thinkingLevelChangeable() {
		return
	}
	level, err := m.app.CycleAgentThinkingLevel(ctx)
	if err != nil {
		m.reportThinkingLevelError("change", err)
		return
	}
	m.status.Thinking = level.String()
}

// handleSetThinkingLevel applies the /effort command: it sets the current
// model's reasoning-effort level to the requested value.
func (m *model) handleSetThinkingLevel(ctx context.Context, level string) {
	if !m.thinkingLevelChangeable() {
		return
	}
	if level == "" {
		m.addNotice("", "Usage: /effort <none|minimal|low|medium|high|xhigh|max>", ui.StMuted())
		return
	}
	parsed, ok := effort.Parse(level)
	if !ok {
		m.addNotice("✗ ", fmt.Sprintf("Unknown effort level %q (valid: none, minimal, low, medium, high, xhigh, max)", level), ui.StError())
		return
	}
	applied, err := m.app.SetAgentThinkingLevel(ctx, parsed)
	if err != nil {
		m.reportThinkingLevelError("set", err)
		return
	}
	m.status.Thinking = applied.String()
	m.addNotice("", "Reasoning effort set to "+applied.String(), ui.StMuted())
}

// thinkingLevelChangeable reports whether the reasoning-effort level can be
// changed, emitting an explanatory notice when it cannot.
func (m *model) thinkingLevelChangeable() bool {
	if m.app == nil {
		return false
	}
	if !m.app.SupportsModelSwitching() {
		m.addNotice("", "Thinking levels can't be changed with remote runtimes", ui.StMuted())
		return false
	}
	return true
}

// reportThinkingLevelError emits a notice for a failed thinking-level change,
// distinguishing the unsupported-model case from other failures.
func (m *model) reportThinkingLevelError(action string, err error) {
	if errors.Is(err, runtime.ErrUnsupported) {
		m.addNotice("", "Current model does not support thinking levels", ui.StMuted())
		return
	}
	m.addNotice("✗ ", fmt.Sprintf("Failed to %s thinking level: %v", action, err), ui.StError())
}

type busySubmitMode int

const (
	busySubmitSteer busySubmitMode = iota
	busySubmitQueue
)

type submitOptions struct {
	fromEditor bool
	busyMode   busySubmitMode
}

func (m *model) submitEditor(ctx context.Context, text string) {
	m.submit(ctx, text, submitOptions{fromEditor: true, busyMode: busySubmitSteer})
}

func (m *model) submitFollowUp(ctx context.Context, text string) {
	m.submit(ctx, text, submitOptions{busyMode: busySubmitQueue})
}

func (m *model) submit(ctx context.Context, text string, opts submitOptions) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return
	}
	if opts.fromEditor {
		m.screen.Editor.RememberHistory(trimmed)
		m.screen.Editor.Reset()
		m.screen.Autocomplete.Dismiss()
	}

	if strings.HasPrefix(trimmed, "/") && m.handleSlash(ctx, trimmed, opts.busyMode) {
		return
	}

	m.dispatchUserMessage(ctx, trimmed, trimmed, opts.busyMode)
}

// handleSlash dispatches a slash command. It returns true when the command was
// fully handled (built-in, skill, or agent command) and false when the input
// should be treated as a normal message.
func (m *model) handleSlash(ctx context.Context, text string, mode busySubmitMode) bool {
	name, rest := splitCommand(text)
	switch name {
	case "exit", "quit":
		m.quit()
		return true
	case "new":
		m.app.NewSession()
		m.resetConversation()
		m.addNotice("", "Started a new session.", ui.StMuted())
		m.refreshCommands(ctx)
		return true
	case "clear":
		m.clearScreen()
		return true
	case "help":
		m.commitHelp()
		return true
	case "compact":
		m.addUserEcho(text)
		m.startCompact(ctx, rest)
		return true
	case "model":
		m.handleModelCommand(ctx, rest)
		return true
	case "effort":
		m.handleSetThinkingLevel(ctx, rest)
		return true
	}

	if skillName, task, ok := m.app.SkillCommandFork(ctx, text); ok {
		m.addUserEcho(text)
		m.startSkillFork(ctx, skillName, task)
		return true
	}

	if _, _, ok := m.app.LookupCommand(ctx, text); ok {
		m.dispatchUserMessage(ctx, text, m.app.ResolveInput(ctx, text), mode)
		return true
	}

	if resolved, err := m.app.ResolveSkillCommand(ctx, text); err == nil && resolved != "" {
		m.dispatchUserMessage(ctx, text, resolved, mode)
		return true
	}

	return false
}

func (m *model) handleModelCommand(ctx context.Context, modelRef string) {
	if m.app == nil || !m.app.SupportsModelSwitching() {
		m.addNotice("", "Model switching is not supported with this runtime", ui.StMuted())
		return
	}

	if modelRef == "" {
		models := m.app.AvailableModels(ctx)
		if len(models) == 0 {
			m.addNotice("", "No models available for selection", ui.StMuted())
			return
		}
		cmds := make([]ui.Command, 0, len(models))
		for _, choice := range models {
			desc := choice.Name
			if choice.IsCurrent {
				desc = strings.TrimSpace(desc + " (current)")
			} else if choice.IsDefault {
				desc = strings.TrimSpace(desc + " (default)")
			}
			value := choice.Ref
			if choice.IsDefault {
				value = "default"
			}
			cmds = append(cmds, ui.Command{
				Name:  choice.Ref,
				Desc:  desc,
				Value: value,
				MatchScore: func(query string) (int, bool) {
					return modelpicker.Score(choice, query)
				},
				Kind: ui.CmdBuiltin,
			})
		}
		m.screen.Autocomplete.SetScopedCommands("model ", cmds)
		m.screen.Editor.SetText("/model ")
		m.screen.Autocomplete.Sync(m.screen.Editor.Text())
		return
	}

	if modelRef == "default" {
		modelRef = ""
	}
	if err := m.app.SetCurrentAgentModel(ctx, modelRef); err != nil {
		m.addNotice("✗ ", "Failed to change model: "+err.Error(), ui.StError())
		return
	}
	m.refreshCommands(ctx)
	if modelRef == "" {
		m.addNotice("", "Model reset to default", ui.StMuted())
		return
	}
	m.addNotice("", "Model changed to "+modelRef, ui.StMuted())
}

func (m *model) dispatchUserMessage(ctx context.Context, display, content string, mode busySubmitMode) {
	if m.app.IsReadOnly() {
		m.addUserEcho(display)
		m.addNotice("⚠ ", "This session is read-only.", ui.StWarning())
		return
	}
	if m.busy {
		if mode == busySubmitSteer {
			if err := m.app.Steer(ctx, runtime.QueuedMessage{Content: content}); err != nil {
				m.addNotice("⚠ ", "Could not steer current response: "+err.Error(), ui.StWarning())
				return
			}
			m.addPendingUser(display, content, ui.PendingUserSteer)
			return
		}
		m.enqueueFollowUp(display, content)
		return
	}
	m.addUserEcho(display)
	m.ignoreUserEcho(content)
	m.startRun(ctx, content, nil)
}

func (m *model) enqueueFollowUp(display, content string) {
	msg := ui.PendingUserMessage{Display: display, Content: content, Kind: ui.PendingUserFollowUp}
	m.queue = append(m.queue, msg)
	m.pendingUsers = append(m.pendingUsers, msg)
}

func (m *model) sendFirstMessage(ctx context.Context, msg, attachPath string) {
	var atts []messages.Attachment
	if attachPath != "" {
		if abs, err := filepath.Abs(attachPath); err == nil {
			atts = append(atts, messages.Attachment{Name: filepath.Base(abs), FilePath: abs})
		}
	}

	trimmed := strings.TrimSpace(msg)
	content := msg
	if strings.HasPrefix(trimmed, "/") {
		if resolved := m.app.ResolveInput(ctx, trimmed); resolved != "" {
			content = resolved
		}
	}

	switch {
	case trimmed != "":
		m.addUserEcho(trimmed)
		m.ignoreUserEcho(content)
	case len(atts) > 0:
		m.addNotice("", "(attached "+atts[0].Name+")", ui.StMuted())
		m.ignoreUserEcho(content)
	default:
		return
	}
	m.startRun(ctx, content, atts)
}

// beginRun marks the model busy and returns a cancelable context for a new
// run, storing its cancel func so it can be interrupted.
func (m *model) beginRun(ctx context.Context) (context.Context, context.CancelFunc) {
	runCtx, cancel := context.WithCancel(ctx)
	m.runCancel = cancel
	m.busy = true
	return runCtx, cancel
}

func (m *model) startRun(ctx context.Context, message string, attachments []messages.Attachment) {
	runCtx, cancel := m.beginRun(ctx)
	m.app.Run(runCtx, cancel, message, attachments)
}

func (m *model) startCompact(ctx context.Context, prompt string) {
	runCtx, cancel := m.beginRun(ctx)
	m.app.CompactSession(runCtx, cancel, prompt)
}

func (m *model) startSkillFork(ctx context.Context, name, task string) {
	runCtx, cancel := m.beginRun(ctx)
	m.app.RunSkillFork(runCtx, cancel, name, task, nil)
}

func (m *model) refreshCommands(ctx context.Context) {
	cmds := make([]ui.Command, 0)
	for _, cmd := range builtinCommands() {
		if !m.disabledCommands[cmd.Name] {
			cmds = append(cmds, cmd)
		}
	}
	for name, c := range m.app.CurrentAgentCommands(ctx) {
		if m.disabledCommands[name] {
			continue
		}
		cmds = append(cmds, ui.Command{Name: name, Desc: c.DisplayText(), Kind: ui.CmdAgent})
	}
	for _, sk := range m.app.CurrentAgentSkills() {
		cmds = append(cmds, ui.Command{Name: sk.Name, Desc: sk.Description, Kind: ui.CmdAgent})
	}
	m.screen.Autocomplete.SetCommands(cmds)
}

func (m *model) handleConfirmKey(k ui.Key) {
	if k.Typ == ui.KeyEsc {
		m.resolveConfirm(runtime.ResumeReject("rejected by user"))
		return
	}
	if k.Typ != ui.KeyRune || len(k.Runes) == 0 {
		return
	}
	switch k.Runes[0] {
	case 'y', 'Y':
		m.resolveConfirm(runtime.ResumeApprove())
	case 'a', 'A':
		m.resolveConfirm(runtime.ResumeApproveTool(m.screen.Confirm.Tool))
	case 's', 'S':
		m.resolveConfirm(runtime.ResumeApproveAutonomous())
	case 'n', 'N':
		m.resolveConfirm(runtime.ResumeReject("rejected by user"))
	}
}

func (m *model) resolveConfirm(req runtime.ResumeRequest) {
	m.app.Resume(req)
	m.screen.Confirm = nil
}

func (m *model) resetConversation() {
	if m.runCancel != nil {
		m.runCancel()
		m.runCancel = nil
	}
	m.screen.Transcript.ClearActive()
	m.queue = nil
	m.pendingUsers = nil
	m.ignoredUsers = nil
	m.busy = false
	m.screen.Confirm = nil
	m.usage.Reset()
	m.status.ContextLength = 0
	m.status.ContextLimit = 0
	m.status.CompactionThreshold = 0
	m.status.Compacting = false
	m.status.Tokens = 0
	m.status.Cost = 0
	m.status.CostKnown = false
}

func (m *model) clearScreen() {
	m.r.Repaint()
}

func (m *model) quit() {
	if m.runCancel != nil {
		m.runCancel()
	}
	m.quitting = true
}

func (m *model) addUserEcho(text string) {
	m.screen.Transcript.AddBlock(func(w int) []string { return ui.RenderUserLines(text, w) })
}

func (m *model) addPendingUser(display, content string, kind ui.PendingUserKind) {
	m.pendingUsers = append(m.pendingUsers, ui.PendingUserMessage{Display: display, Content: content, Kind: kind})
}

func (m *model) consumePendingUser(kind ui.PendingUserKind, content string) (ui.PendingUserMessage, bool) {
	for i, msg := range m.pendingUsers {
		if msg.Kind != kind || !samePendingUserContent(msg.Content, content) {
			continue
		}
		m.pendingUsers = append(m.pendingUsers[:i], m.pendingUsers[i+1:]...)
		return msg, true
	}
	return ui.PendingUserMessage{}, false
}

func samePendingUserContent(pending, emitted string) bool {
	return pending == emitted || pending == strings.TrimSuffix(emitted, "\n")
}

func (m *model) ignoreUserEcho(content string) {
	m.ignoredUsers = append(m.ignoredUsers, content)
}

func (m *model) consumeIgnoredUserEcho(content string) bool {
	for i, msg := range m.ignoredUsers {
		if msg != content {
			continue
		}
		m.ignoredUsers = append(m.ignoredUsers[:i], m.ignoredUsers[i+1:]...)
		return true
	}
	return false
}

func (m *model) addNotice(prefix, text string, style lipgloss.Style) {
	m.screen.Transcript.AddBlock(func(w int) []string { return ui.RenderNoticeLines(prefix, text, w, style) })
}

func (m *model) commitHelp() {
	m.screen.Transcript.AddBlock(func(int) []string {
		return []string{
			ui.StBold().Render("Commands"),
			ui.StMuted().Render("  /new       start a new session"),
			ui.StMuted().Render("  /compact   summarize and compact the conversation"),
			ui.StMuted().Render("  /model     change the model for the current agent"),
			ui.StMuted().Render("  /effort    set the model's reasoning effort (e.g. /effort high)"),
			ui.StMuted().Render("  /clear     clear the screen"),
			ui.StMuted().Render("  /help      show this help"),
			ui.StMuted().Render("  /exit      quit"),
			"",
			ui.StBold().Render("Shortcuts"),
			ui.StMuted().Render("  Enter      send             Alt+Enter   insert newline"),
			ui.StMuted().Render("  Up/Down    history           Tab         complete command"),
			ui.StMuted().Render("  Shift+Tab  cycle thinking    Ctrl+C      cancel / quit"),
			ui.StMuted().Render("  Ctrl+W     delete previous word"),
		}
	})
}

func splitCommand(text string) (name, rest string) {
	text = strings.TrimPrefix(strings.TrimSpace(text), "/")
	name, rest, _ = strings.Cut(text, " ")
	return name, strings.TrimSpace(rest)
}
