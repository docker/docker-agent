//go:build js && wasm

package main

// runtime_wasm.go implements a lightweight agentic loop for the browser-based
// wasm build. It provides:
//   - Tool calling loop (model requests tools → we execute → send results back)
//   - Multi-agent handoffs (transfer_task / handoff)
//   - Remote MCP tool support (HTTP/SSE transport)
//   - Hooks (session_start, turn_start, pre_tool_use, post_tool_use)
//   - Fallback models with retry + backoff
//
// It intentionally does NOT use pkg/runtime (blocked by sqlite) or
// pkg/teamloader. Instead, it builds agents from config using pkg/agent
// directly, constructs an in-process hook executor, and runs a simplified
// version of the streaming loop.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"syscall/js"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

// maxIterations prevents infinite tool-call loops.
const maxIterations = 50

// wasmRuntime holds the state for one chat session.
type wasmRuntime struct {
	cfg          *latest.Config
	env          environment.Provider
	team         *team.Team
	agents       map[string]*agent.Agent
	providers    map[string]provider.Provider
	fallbacks    map[string][]provider.Provider
	hookExec     map[string]*hooks.Executor
	currentAgent string
	onEvent      js.Value
}

// buildRuntime constructs a team of agents from the config, with providers,
// MCP toolsets, hooks, and fallback models wired up.
func buildRuntime(ctx context.Context, cfg *latest.Config, env environment.Provider, onEvent js.Value) (*wasmRuntime, error) {
	rt := &wasmRuntime{
		cfg:       cfg,
		env:       env,
		agents:    make(map[string]*agent.Agent),
		providers: make(map[string]provider.Provider),
		fallbacks: make(map[string][]provider.Provider),
		hookExec:  make(map[string]*hooks.Executor),
		onEvent:   onEvent,
	}

	// Build providers for each agent.
	for _, agentCfg := range cfg.Agents {
		modelCfg, err := resolveModel(cfg, agentCfg.Model)
		if err != nil {
			return nil, fmt.Errorf("agent %q: %w", agentCfg.Name, err)
		}

		prov, err := provider.NewWithModels(ctx, &modelCfg, cfg.Models, env, options.WithProviders(cfg.Providers))
		if err != nil {
			return nil, fmt.Errorf("agent %q: building model provider: %w", agentCfg.Name, err)
		}
		rt.providers[agentCfg.Name] = prov

		// Build fallback providers.
		for _, fbModel := range agentCfg.GetFallbackModels() {
			fbCfg, err := resolveModel(cfg, fbModel)
			if err != nil {
				slog.WarnContext(ctx, "Skipping fallback model", "agent", agentCfg.Name, "model", fbModel, "error", err)
				continue
			}
			fbProv, err := provider.NewWithModels(ctx, &fbCfg, cfg.Models, env, options.WithProviders(cfg.Providers))
			if err != nil {
				slog.WarnContext(ctx, "Skipping fallback model", "agent", agentCfg.Name, "model", fbModel, "error", err)
				continue
			}
			rt.fallbacks[agentCfg.Name] = append(rt.fallbacks[agentCfg.Name], fbProv)
		}
	}

	// Build agent objects. Two passes: first create all agents, then wire
	// handoffs and sub-agents (which require the targets to exist already).
	type agentBuildInfo struct {
		name     string
		opts     []agent.Opt
		handoffs []string
		subs     []string
	}
	var buildInfos []agentBuildInfo

	for _, agentCfg := range cfg.Agents {
		opts := []agent.Opt{
			agent.WithModel(rt.providers[agentCfg.Name]),
			agent.WithDescription(agentCfg.Description),
		}

		// Fallback models.
		for _, fb := range rt.fallbacks[agentCfg.Name] {
			opts = append(opts, agent.WithFallbackModel(fb))
		}

		// Toolsets: filesystem, remote MCP, etc.
		for _, ts := range agentCfg.Toolsets {
			switch {
			case ts.Type == "filesystem" || ts.Type == "":
				// Default toolset type is filesystem.
				if ts.Type == "filesystem" {
					fsTool := filesystem.NewFilesystemTool("/")
					opts = append(opts, agent.WithToolSets(fsTool))
				}
			case ts.Remote.URL != "":
				transport := ts.Remote.TransportType
				if transport == "" {
					transport = "streamable-http"
				}
				mcpTS := mcptools.NewRemoteToolset(
					ts.Name,
					ts.Remote.URL,
					transport,
					ts.Remote.Headers,
					ts.Remote.OAuth,
				)
				opts = append(opts, agent.WithToolSets(mcpTS))
			case ts.URL != "":
				// URL directly on the toolset (legacy format)
				transport := "streamable-http"
				mcpTS := mcptools.NewRemoteToolset(
					ts.Name,
					ts.URL,
					transport,
					ts.Headers,
					nil,
				)
				opts = append(opts, agent.WithToolSets(mcpTS))
			}
		}

		// Hooks.
		if agentCfg.Hooks != nil {
			opts = append(opts, agent.WithHooks(agentCfg.Hooks))

			// Build a hook executor for this agent.
			registry := hooks.NewRegistry()
			builtins.Register(registry)
			hookExec := hooks.NewExecutorWithRegistry(agentCfg.Hooks, "", nil, registry)
			rt.hookExec[agentCfg.Name] = hookExec
		}

		buildInfos = append(buildInfos, agentBuildInfo{
			name:     agentCfg.Name,
			opts:     opts,
			handoffs: agentCfg.Handoffs,
			subs:     agentCfg.SubAgents,
		})
	}

	// First pass: create all agent objects (without cross-references).
	agentsByName := make(map[string]*agent.Agent)
	for _, info := range buildInfos {
		a := agent.New(info.name, rt.agentInstruction(info.name), info.opts...)
		agentsByName[info.name] = a
		rt.agents[info.name] = a
	}

	// Second pass: wire handoffs and sub-agents. Since agent.WithHandoffs
	// and agent.WithSubAgents are construction-time options, we rebuild
	// agents that have cross-references.
	for _, info := range buildInfos {
		needsRebuild := false
		var extraOpts []agent.Opt

		if len(info.handoffs) > 0 {
			var handoffs []*agent.Agent
			for _, h := range info.handoffs {
				if ha, ok := agentsByName[h]; ok {
					handoffs = append(handoffs, ha)
				}
			}
			if len(handoffs) > 0 {
				extraOpts = append(extraOpts, agent.WithHandoffs(handoffs...))
				needsRebuild = true
			}
		}

		if len(info.subs) > 0 {
			var subAgents []*agent.Agent
			for _, s := range info.subs {
				if sa, ok := agentsByName[s]; ok {
					subAgents = append(subAgents, sa)
				}
			}
			if len(subAgents) > 0 {
				extraOpts = append(extraOpts, agent.WithSubAgents(subAgents...))
				needsRebuild = true
			}
		}

		if needsRebuild {
			allOpts := append(info.opts, extraOpts...)
			a := agent.New(info.name, rt.agentInstruction(info.name), allOpts...)
			agentsByName[info.name] = a
			rt.agents[info.name] = a
		}
	}

	// Build team.
	var allAgents []*agent.Agent
	for _, agentCfg := range cfg.Agents {
		allAgents = append(allAgents, agentsByName[agentCfg.Name])
	}
	rt.team = team.New(team.WithAgents(allAgents...))

	return rt, nil
}

// agentInstruction returns the instruction for the named agent from cfg.
func (rt *wasmRuntime) agentInstruction(name string) string {
	for _, a := range rt.cfg.Agents {
		if a.Name == name {
			return a.Instruction
		}
	}
	return ""
}

// runAgentLoop runs the full agentic loop: stream completions, process tool
// calls, handle handoffs, and loop until the model says stop or we hit
// maxIterations.
func (rt *wasmRuntime) runAgentLoop(ctx context.Context, agentName string, messages []chat.Message) (map[string]any, error) {
	if agentName == "" {
		if len(rt.cfg.Agents) == 1 {
			agentName = rt.cfg.Agents[0].Name
		} else {
			agentName = rt.cfg.Agents[0].Name
		}
	}
	rt.currentAgent = agentName

	a, ok := rt.agents[rt.currentAgent]
	if !ok {
		return nil, fmt.Errorf("agent %q not found", rt.currentAgent)
	}

	// Prepend system instruction.
	if a.Instruction() != "" && (len(messages) == 0 || messages[0].Role != chat.MessageRoleSystem) {
		sys := chat.Message{Role: chat.MessageRoleSystem, Content: a.Instruction()}
		messages = append([]chat.Message{sys}, messages...)
	}

	// Fire session_start hook.
	rt.fireHook(ctx, rt.currentAgent, hooks.EventSessionStart, &hooks.Input{})

	var totalUsage chat.Usage
	iteration := 0

	for iteration < maxIterations {
		iteration++

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		a = rt.agents[rt.currentAgent]

		// Fire turn_start hook.
		turnCtx := rt.fireHook(ctx, rt.currentAgent, hooks.EventTurnStart, &hooks.Input{})
		if turnCtx != nil && turnCtx.AdditionalContext != "" {
			// Inject context as a transient system message.
			ctxMsg := chat.Message{Role: chat.MessageRoleSystem, Content: turnCtx.AdditionalContext}
			messages = append(messages, ctxMsg)
		}

		// Get tools for this agent.
		agentTools, err := a.Tools(ctx)
		if err != nil {
			slog.WarnContext(ctx, "Failed to get tools", "agent", rt.currentAgent, "error", err)
			agentTools = nil
		}

		// Add delegation tools if agent has handoffs or sub-agents.
		agentTools = rt.addDelegationTools(a, agentTools)

		// Try primary model with fallback chain.
		streamResult, err := rt.streamWithFallback(ctx, a, messages, agentTools)
		if err != nil {
			return nil, fmt.Errorf("model call failed: %w", err)
		}

		// Accumulate usage.
		if streamResult.usage != nil {
			totalUsage.InputTokens += streamResult.usage.InputTokens
			totalUsage.OutputTokens += streamResult.usage.OutputTokens
		}

		// Record assistant message in conversation history.
		assistantMsg := chat.Message{
			Role:             chat.MessageRoleAssistant,
			Content:          streamResult.content,
			ReasoningContent: streamResult.reasoning,
			ToolCalls:        streamResult.toolCalls,
		}
		messages = append(messages, assistantMsg)

		// No tool calls → model is done.
		if len(streamResult.toolCalls) == 0 {
			rt.emitEvent(map[string]any{"type": "finish", "reason": string(streamResult.finishReason)})
			break
		}

		// Process tool calls.
		toolMessages, handoff, err := rt.processToolCalls(ctx, streamResult.toolCalls, agentTools, messages)
		if err != nil {
			return nil, fmt.Errorf("processing tool calls: %w", err)
		}
		messages = append(messages, toolMessages...)

		// Handle agent handoff.
		if handoff != "" {
			rt.emitEvent(map[string]any{
				"type": "handoff",
				"from": rt.currentAgent,
				"to":   handoff,
			})
			rt.currentAgent = handoff
			// Update system instruction for new agent.
			newAgent := rt.agents[handoff]
			if newAgent.Instruction() != "" {
				sysMsg := chat.Message{
					Role:    chat.MessageRoleSystem,
					Content: newAgent.Instruction(),
				}
				messages = append(messages, sysMsg)
			}
		}
	}

	// Emit final usage.
	if totalUsage.InputTokens > 0 || totalUsage.OutputTokens > 0 {
		rt.emitEvent(map[string]any{
			"type":          "usage",
			"input_tokens":  float64(totalUsage.InputTokens),
			"output_tokens": float64(totalUsage.OutputTokens),
		})
	}

	// Build result.
	result := map[string]any{
		"message": map[string]any{
			"role":    "assistant",
			"content": rt.lastAssistantContent(messages),
		},
	}
	if totalUsage.InputTokens > 0 || totalUsage.OutputTokens > 0 {
		result["usage"] = map[string]any{
			"input_tokens":  float64(totalUsage.InputTokens),
			"output_tokens": float64(totalUsage.OutputTokens),
		}
	}
	return result, nil
}

// streamResult holds the aggregated result of one completion call.
type wasmStreamResult struct {
	content      string
	reasoning    string
	toolCalls    []tools.ToolCall
	finishReason chat.FinishReason
	usage        *chat.Usage
}

// streamWithFallback tries the primary model, then fallbacks on failure.
func (rt *wasmRuntime) streamWithFallback(ctx context.Context, a *agent.Agent, messages []chat.Message, agentTools []tools.Tool) (*wasmStreamResult, error) {
	prov := rt.providers[a.Name()]
	fallbacks := rt.fallbacks[a.Name()]

	chain := append([]provider.Provider{prov}, fallbacks...)

	var lastErr error
	for i, p := range chain {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		if i > 0 {
			rt.emitEvent(map[string]any{
				"type":    "fallback",
				"from":    chain[i-1].ID(),
				"to":      p.ID(),
				"attempt": float64(i + 1),
				"reason":  lastErr.Error(),
			})
			// Brief backoff between fallback attempts.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(i) * 500 * time.Millisecond):
			}
		}

		result, err := rt.streamCompletion(ctx, p, a, messages, agentTools)
		if err != nil {
			lastErr = err
			slog.WarnContext(ctx, "Model attempt failed", "model", p.ID(), "attempt", i+1, "error", err)
			continue
		}
		return result, nil
	}

	return nil, fmt.Errorf("all models failed: %w", lastErr)
}

// streamCompletion runs one streaming completion call and emits deltas.
func (rt *wasmRuntime) streamCompletion(ctx context.Context, prov provider.Provider, a *agent.Agent, messages []chat.Message, agentTools []tools.Tool) (*wasmStreamResult, error) {
	stream, err := prov.CreateChatCompletionStream(ctx, messages, agentTools)
	if err != nil {
		return nil, fmt.Errorf("opening stream: %w", err)
	}
	defer stream.Close()

	var content strings.Builder
	var reasoning strings.Builder
	var toolCalls []tools.ToolCall
	var usage *chat.Usage
	var finishReason chat.FinishReason

	toolCallIndex := make(map[string]int)

	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading stream: %w", err)
		}

		if resp.Usage != nil {
			usage = resp.Usage
		}

		for _, choice := range resp.Choices {
			// Tool call deltas.
			if len(choice.Delta.ToolCalls) > 0 {
				for _, delta := range choice.Delta.ToolCalls {
					idx, exists := toolCallIndex[delta.ID]
					if !exists {
						idx = len(toolCalls)
						toolCallIndex[delta.ID] = idx
						toolCalls = append(toolCalls, tools.ToolCall{
							ID:   delta.ID,
							Type: delta.Type,
						})
					}
					tc := &toolCalls[idx]
					if delta.Type != "" {
						tc.Type = delta.Type
					}
					if delta.Function.Name != "" {
						tc.Function.Name = delta.Function.Name
					}
					if delta.Function.Arguments != "" {
						tc.Function.Arguments += delta.Function.Arguments
					}

					// Emit partial tool call event.
					if tc.Function.Name != "" {
						rt.emitEvent(map[string]any{
							"type":      "tool_call_delta",
							"id":        tc.ID,
							"name":      tc.Function.Name,
							"arguments": delta.Function.Arguments,
						})
					}
				}
				continue
			}

			// Content delta.
			if choice.Delta.Content != "" {
				content.WriteString(choice.Delta.Content)
				rt.emitEvent(map[string]any{"type": "delta", "content": choice.Delta.Content})
			}

			// Reasoning delta.
			if choice.Delta.ReasoningContent != "" {
				reasoning.WriteString(choice.Delta.ReasoningContent)
				rt.emitEvent(map[string]any{"type": "delta", "reasoning": choice.Delta.ReasoningContent})
			}

			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
		}
	}

	// Infer finish reason if not set.
	if finishReason == "" {
		if len(toolCalls) > 0 {
			finishReason = chat.FinishReasonToolCalls
		} else {
			finishReason = chat.FinishReasonStop
		}
	}

	return &wasmStreamResult{
		content:      content.String(),
		reasoning:    reasoning.String(),
		toolCalls:    toolCalls,
		finishReason: finishReason,
		usage:        usage,
	}, nil
}

// processToolCalls executes tool calls and returns the tool result messages.
// Returns (messages, handoffAgentName, error).
func (rt *wasmRuntime) processToolCalls(ctx context.Context, calls []tools.ToolCall, agentTools []tools.Tool, _ []chat.Message) ([]chat.Message, string, error) {
	toolByName := make(map[string]tools.Tool, len(agentTools))
	for _, t := range agentTools {
		toolByName[t.Name] = t
	}

	var resultMessages []chat.Message
	var handoffAgent string

	for _, tc := range calls {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}

		// Emit tool_call event.
		rt.emitEvent(map[string]any{
			"type": "tool_call",
			"id":   tc.ID,
			"name": tc.Function.Name,
			"args": tc.Function.Arguments,
		})

		// Fire pre_tool_use hook.
		preResult := rt.fireHook(ctx, rt.currentAgent, hooks.EventPreToolUse, &hooks.Input{
			ToolName:  tc.Function.Name,
			ToolUseID: tc.ID,
			ToolInput: parseToolArgs(tc.Function.Arguments),
		})
		if preResult != nil && !preResult.Allowed {
			// Hook denied the tool call.
			rt.emitEvent(map[string]any{
				"type":   "tool_blocked",
				"id":     tc.ID,
				"name":   tc.Function.Name,
				"reason": preResult.Message,
			})
			resultMessages = append(resultMessages, chat.Message{
				Role:       chat.MessageRoleTool,
				ToolCallID: tc.ID,
				Content:    fmt.Sprintf("Tool call denied by hook: %s", preResult.Message),
			})
			continue
		}

		// Handle built-in delegation tools.
		switch tc.Function.Name {
		case "transfer_task":
			result, target := rt.handleTransferTask(tc)
			resultMessages = append(resultMessages, chat.Message{
				Role:       chat.MessageRoleTool,
				ToolCallID: tc.ID,
				Content:    result,
			})
			if target != "" {
				handoffAgent = target
			}
			continue
		case "handoff":
			result, target := rt.handleHandoff(tc)
			resultMessages = append(resultMessages, chat.Message{
				Role:       chat.MessageRoleTool,
				ToolCallID: tc.ID,
				Content:    result,
			})
			if target != "" {
				handoffAgent = target
			}
			continue
		}

		// Execute via toolset.
		tool, exists := toolByName[tc.Function.Name]
		if !exists {
			errMsg := fmt.Sprintf("Tool %q not found", tc.Function.Name)
			resultMessages = append(resultMessages, chat.Message{
				Role:       chat.MessageRoleTool,
				ToolCallID: tc.ID,
				Content:    errMsg,
			})
			continue
		}

		toolResult, err := tool.Handler(ctx, tc)
		var output string
		if err != nil {
			output = fmt.Sprintf("Error: %v", err)
		} else if toolResult != nil {
			output = toolResult.Output
		}

		// Emit tool result event.
		rt.emitEvent(map[string]any{
			"type":   "tool_result",
			"id":     tc.ID,
			"name":   tc.Function.Name,
			"output": truncateOutput(output, 500),
		})

		// Fire post_tool_use hook.
		rt.fireHook(ctx, rt.currentAgent, hooks.EventPostToolUse, &hooks.Input{
			ToolName:     tc.Function.Name,
			ToolUseID:    tc.ID,
			ToolInput:    parseToolArgs(tc.Function.Arguments),
			ToolResponse: output,
			ToolError:    err != nil,
		})

		resultMessages = append(resultMessages, chat.Message{
			Role:       chat.MessageRoleTool,
			ToolCallID: tc.ID,
			Content:    output,
		})
	}

	return resultMessages, handoffAgent, nil
}

// addDelegationTools injects transfer_task and/or handoff tools into the
// tool list when the agent has sub-agents or handoffs configured.
func (rt *wasmRuntime) addDelegationTools(a *agent.Agent, existingTools []tools.Tool) []tools.Tool {
	if len(a.Handoffs()) > 0 {
		var names []string
		for _, h := range a.Handoffs() {
			names = append(names, h.Name())
		}
		existingTools = append(existingTools, tools.Tool{
			Name:        "handoff",
			Description: fmt.Sprintf("Hand off the conversation to another agent. Available agents: %s", strings.Join(names, ", ")),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"agent": map[string]any{
						"type":        "string",
						"description": "The name of the agent to hand off to",
						"enum":        names,
					},
				},
				"required": []string{"agent"},
			},
		})
	}

	if len(a.SubAgents()) > 0 {
		var names []string
		var descriptions []string
		for _, s := range a.SubAgents() {
			names = append(names, s.Name())
			descriptions = append(descriptions, fmt.Sprintf("- %s: %s", s.Name(), s.Description()))
		}
		existingTools = append(existingTools, tools.Tool{
			Name:        "transfer_task",
			Description: fmt.Sprintf("Transfer a task to a sub-agent. Available agents:\n%s", strings.Join(descriptions, "\n")),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"agent": map[string]any{
						"type":        "string",
						"description": "The name of the agent to transfer to",
						"enum":        names,
					},
					"task": map[string]any{
						"type":        "string",
						"description": "The task to perform",
					},
					"expected_output": map[string]any{
						"type":        "string",
						"description": "Expected output from the agent",
					},
				},
				"required": []string{"agent", "task"},
			},
		})
	}

	return existingTools
}

// handleTransferTask processes the transfer_task tool call.
func (rt *wasmRuntime) handleTransferTask(tc tools.ToolCall) (string, string) {
	var params struct {
		Agent          string `json:"agent"`
		Task           string `json:"task"`
		ExpectedOutput string `json:"expected_output"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &params); err != nil {
		return fmt.Sprintf("Invalid arguments: %v", err), ""
	}

	if _, ok := rt.agents[params.Agent]; !ok {
		return fmt.Sprintf("Agent %q not found", params.Agent), ""
	}

	return fmt.Sprintf("Task transferred to %s: %s", params.Agent, params.Task), params.Agent
}

// handleHandoff processes the handoff tool call.
func (rt *wasmRuntime) handleHandoff(tc tools.ToolCall) (string, string) {
	var params struct {
		Agent string `json:"agent"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &params); err != nil {
		return fmt.Sprintf("Invalid arguments: %v", err), ""
	}

	if _, ok := rt.agents[params.Agent]; !ok {
		return fmt.Sprintf("Agent %q not found", params.Agent), ""
	}

	return fmt.Sprintf("Conversation handed off to %s. The agent %s handed off the conversation to you. "+
		"Complete your part of the task and hand off to the next appropriate agent "+
		"(if any are available to you), or respond directly to the user.", params.Agent, rt.currentAgent), params.Agent
}

// fireHook dispatches a hook event and returns the result.
func (rt *wasmRuntime) fireHook(ctx context.Context, agentName string, event hooks.EventType, input *hooks.Input) *hooks.Result {
	exec, ok := rt.hookExec[agentName]
	if !ok || exec == nil {
		return nil
	}
	if !exec.Has(event) {
		return nil
	}

	result, err := exec.Dispatch(ctx, event, input)
	if err != nil {
		slog.WarnContext(ctx, "Hook dispatch failed", "event", event, "agent", agentName, "error", err)
		return nil
	}
	return result
}

// emitEvent sends a typed event to the JS onEvent callback.
func (rt *wasmRuntime) emitEvent(event map[string]any) {
	emit(rt.onEvent, event)
}

// lastAssistantContent returns the content of the last assistant message.
func (rt *wasmRuntime) lastAssistantContent(messages []chat.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == chat.MessageRoleAssistant && messages[i].Content != "" {
			return messages[i].Content
		}
	}
	return ""
}

// parseToolArgs attempts to parse a JSON string into a map.
func parseToolArgs(args string) map[string]any {
	var result map[string]any
	if err := json.Unmarshal([]byte(args), &result); err != nil {
		return nil
	}
	return result
}

// truncateOutput truncates a string to maxLen characters, appending "…" if truncated.
func truncateOutput(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}
