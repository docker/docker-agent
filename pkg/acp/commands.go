package acp

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/runtime"
)

var executableCommand = regexp.MustCompile("!([a-zA-Z0-9_]+\\(|`)")

func supportedCommand(cmd types.Command) bool {
	return cmd.URL == "" && !strings.Contains(cmd.Instruction, "${") && !executableCommand.MatchString(cmd.Instruction)
}

func commandInput(hint string) *acp.AvailableCommandInput {
	return &acp.AvailableCommandInput{Unstructured: &acp.UnstructuredCommandInput{Hint: hint}}
}

func availableCommands(ctx context.Context, rt runtime.Runtime) []acp.AvailableCommand {
	commands := []acp.AvailableCommand{
		{Name: "compact", Description: "Summarize and compact session history", Input: commandInput("Optional compaction instructions")},
		{Name: "usage", Description: "Display current context token usage and session cost"},
	}
	configured := rt.CurrentAgentInfo(ctx).Commands
	names := make([]string, 0, len(configured))
	for name, cmd := range configured {
		if name == "" || (strings.Contains(name, "/") || strings.IndexFunc(name, unicode.IsSpace) >= 0) || name == "new" || name == "compact" || name == "usage" || !supportedCommand(cmd) {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		commands = append(commands, acp.AvailableCommand{Name: name, Description: configured[name].DisplayText(), Input: commandInput("Optional arguments")})
	}
	return commands
}

func (a *Agent) emitAvailableCommands(ctx context.Context, s *Session) error {
	if a.conn == nil || s.rt == nil {
		return nil
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	return a.sendUpdate(ctx, s.id, acp.SessionUpdate{AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
		SessionUpdate: "available_commands_update", AvailableCommands: availableCommands(ctx, s.rt),
	}})
}

func (a *Agent) refreshCommands(ctx context.Context, s *Session) {
	if err := a.emitAvailableCommands(ctx, s); err != nil {
		slog.DebugContext(ctx, "Failed to emit available commands", "error", err)
	}
}

// dispatchCommand only interprets directly supplied leading text, never attached resource contents.
func (a *Agent) dispatchCommand(ctx context.Context, s *Session, prompt []acp.ContentBlock) ([]acp.ContentBlock, bool, error) {
	var leading strings.Builder
	count := 0
	for _, block := range prompt {
		if block.Text == nil {
			break
		}
		leading.WriteString(block.Text.Text)
		count++
	}
	name, ok := strings.CutPrefix(leading.String(), "/")
	if !ok {
		return prompt, false, nil
	}
	args := ""
	if i := strings.IndexFunc(name, unicode.IsSpace); i >= 0 {
		_, width := utf8.DecodeRuneInString(name[i:])
		args = name[i+width:]
		name = name[:i]
	}
	attachments := prompt[count:]
	switch name {
	case "new":
		return nil, true, acp.NewInvalidParams("/new is not supported; use session/new to start a new conversation")
	case "usage", "compact":
		if len(attachments) != 0 || (name == "usage" && strings.TrimSpace(args) != "") {
			return nil, true, acp.NewInvalidParams("/" + name + " does not accept attachments or these arguments")
		}
		a.refreshCommands(ctx, s)
		if name == "usage" {
			return nil, true, a.showUsage(ctx, s)
		}
		return nil, true, a.compactSession(ctx, s, strings.TrimSpace(args))
	}

	cmd, ok := s.rt.CurrentAgentInfo(ctx).Commands[name]
	if !ok {
		return prompt, false, nil
	}
	if !supportedCommand(cmd) {
		return nil, true, acp.NewInvalidParams("this command requires URL opening or dynamic expansion, which ACP does not support")
	}
	resolved := cmd.Instruction
	if args != "" {
		if resolved != "" {
			resolved += " "
		}
		resolved += args
	}
	if cmd.Agent != "" {
		if err := s.rt.SetCurrentAgent(ctx, cmd.Agent); err != nil {
			return nil, true, acp.NewInvalidParams(fmt.Sprintf("cannot switch agent: %s", err))
		}
		a.refreshCommands(ctx, s)
		a.refreshConfiguration(ctx, s)
	}
	if resolved == "" && len(attachments) == 0 {
		return nil, true, nil
	}
	result := make([]acp.ContentBlock, 0, 1+len(attachments))
	if resolved != "" {
		result = append(result, acp.TextBlock(resolved))
	}
	return append(result, attachments...), false, nil
}

func (a *Agent) showUsage(ctx context.Context, s *Session) error {
	usage := s.currentUsage(s.rt.CurrentAgentName(ctx))
	limit := usage.ContextLimit
	if err := a.emitUsage(ctx, s.id, usage); err != nil {
		return err
	}
	capacity := "context limit unknown"
	if limit > 0 {
		capacity = fmt.Sprintf("last reported limit %d", limit)
	}
	return a.sendUpdate(ctx, s.id, acp.UpdateAgentMessageText(fmt.Sprintf("Current context: %d tokens (%s). Session cost: $%.6f USD.", usage.ContextLength, capacity, usage.Cost)))
}

func (a *Agent) emitUsage(ctx context.Context, sid string, usage *runtime.Usage) error {
	if usage == nil || usage.ContextLimit <= 0 {
		return nil
	}
	update := acp.SessionUsageUpdate{SessionUpdate: "usage_update", Size: int(usage.ContextLimit), Used: int(usage.ContextLength)}
	if usage.Cost > 0 {
		update.Cost = &acp.Cost{Amount: usage.Cost, Currency: "USD"}
	}
	return a.sendUpdate(ctx, sid, acp.SessionUpdate{UsageUpdate: &update})
}

func (a *Agent) compactSession(ctx context.Context, s *Session, instruction string) error {
	compactCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var resultErr error
	outcome := runtime.CompactionOutcomeSkipped
	s.rt.Summarize(compactCtx, s.sess, instruction, runtime.EventSinkFunc(func(event runtime.Event) {
		mu.Lock()
		defer mu.Unlock()
		var usage *runtime.Usage
		if e, ok := event.(*runtime.TokenUsageEvent); ok {
			usage = s.recordUsage(e)
		}
		if resultErr != nil || compactCtx.Err() != nil {
			return
		}
		if scoped, ok := event.(runtime.SessionScoped); ok && scoped.GetSessionID() != s.sess.ID {
			return
		}
		switch e := event.(type) {
		case *runtime.ErrorEvent:
			resultErr = runtimePromptError(s.sess.ID, "compaction_failed", e.Error)
		case *runtime.SessionCompactionEvent:
			if e.Status == "completed" {
				outcome = e.Outcome
			}
		case *runtime.TokenUsageEvent:
			resultErr = a.emitUsage(compactCtx, s.id, usage)
		case *runtime.WarningEvent:
			resultErr = a.sendUpdate(compactCtx, s.id, acp.UpdateAgentMessageText("Warning: "+e.Message))
		}
		if resultErr != nil {
			cancel()
		}
	}))
	mu.Lock()
	defer mu.Unlock()
	if resultErr != nil {
		return resultErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	message := "Compaction skipped."
	switch outcome {
	case runtime.CompactionOutcomeApplied, "":
		message = "Session history compacted."
	case runtime.CompactionOutcomeFailed:
		return runtimePromptError(s.sess.ID, "compaction_failed", "Session compaction failed")
	}
	return a.sendUpdate(ctx, s.id, acp.UpdateAgentMessageText(message))
}
