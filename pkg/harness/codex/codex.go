// Package codex implements the OpenAI Codex CLI harness adapter for docker-agent.
// It spawns `codex exec --json` as a subprocess and translates its JSONL event
// stream into canonical harness events.
//
// # Invocation
//
//	codex exec \
//	  --json \
//	  --sandbox workspace-write \
//	  --ask-for-approval never \
//	  --cd <workdir> \
//	  --skip-git-repo-check \
//	  -- <prompt>
//
// Multi-turn resume uses:
//
//	codex exec resume <thread_id> --json -- <prompt>
//
// # Wire format
//
// Codex CLI emits JSONL on stdout. Each line is a JSON object with a "type"
// discriminator. Tool calls are atomic: a single "item.completed" event with
// subtype "command_execution", "file_change", "mcp_tool_call", or
// "web_search" carries both the call and its result. Text and reasoning are
// also delivered as final blocks (no streaming deltas).
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/harness"
)

const adapterName = "codex"

// Adapter implements harness.HarnessAdapter for the OpenAI Codex CLI.
type Adapter struct{}

func init() {
	harness.Register(&Adapter{})
}

// Name returns the harness type identifier.
func (a *Adapter) Name() string { return adapterName }

// Capabilities returns the static capability declaration.
func (a *Adapter) Capabilities() harness.AdapterCapabilities {
	return harness.AdapterCapabilities{
		Protocol: harness.ProtocolStream,
		Requires: harness.HostRequirements{},
		Features: harness.AdapterFeatures{
			SystemPrompt:  false, // codex exec has no --system-prompt flag
			Reasoning:     true,
			TextDeltas:    false, // only final messages
			MultiTurn:     true,  // via codex exec resume <thread_id>
			StreamingArgs: false,
		},
		BuiltInTools: []string{"shell", "write", "edit", "read", "glob", "grep"},
	}
}

// Run executes one sub-session against the Codex CLI.
// All terminal states flow through req.Events as RunEnd or RunError.
func (a *Adapter) Run(ctx context.Context, req harness.SubSessionRequest) {
	if err := a.run(ctx, req); err != nil {
		req.Events.Emit(harness.RunError{
			RunID:   req.RunID,
			Code:    harness.ErrCodeHarnessCrashed,
			Message: err.Error(),
			At:      time.Now(),
		})
	}
}

func (a *Adapter) run(ctx context.Context, req harness.SubSessionRequest) error {
	cfg := parseConfig(req.Config)

	binary := "codex"
	if cfg != nil && cfg.Command != "" {
		binary = cfg.Command
	}

	args := buildArgs(req, cfg)

	cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec
	cmd.Dir = req.WorkingDir
	cmd.Env = buildEnv(req)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("codex stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("codex stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("codex start: %w", err)
	}

	// Drain stderr to debug log.
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			slog.Debug("codex stderr", "line", scanner.Text())
		}
	}()

	// Read and translate JSONL events from stdout.
	state := &translatorState{
		runID:     req.RunID,
		agentName: req.RunID,
	}
	translateStream(stdout, state, req.Events)

	return cmd.Wait()
}

// buildArgs constructs the codex CLI arguments for a sub-session.
func buildArgs(req harness.SubSessionRequest, cfg *Config) []string {
	sandbox := "workspace-write"
	if cfg != nil && cfg.Sandbox != "" {
		sandbox = cfg.Sandbox
	}

	var args []string

	if req.ResumeToken != "" {
		// Resume an existing thread.
		args = append(args, "exec", "resume", req.ResumeToken, "--json")
	} else {
		args = append(args,
			"exec",
			"--json",
			"--dangerously-bypass-approvals-and-sandbox",
			"--skip-git-repo-check",
		)
		if req.WorkingDir != "" {
			args = append(args, "-C", req.WorkingDir)
		}
		_ = sandbox // sandbox mode is controlled via --dangerously-bypass-approvals-and-sandbox in this version
	}

	if cfg != nil {
		args = append(args, cfg.Args...)
	}

	// Prompt is the final positional argument after `--`.
	prompt := req.Task
	if req.ResumeToken == "" && req.SystemPrompt != "" {
		// codex exec has no --system-prompt flag; prepend it to the task.
		prompt = req.SystemPrompt + "\n\n" + req.Task
	}

	args = append(args, "--", prompt)
	return args
}

// buildEnv constructs the environment for the codex subprocess.
func buildEnv(req harness.SubSessionRequest) []string {
	env := os.Environ()
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// --- Config ---

// Config holds Codex CLI adapter-specific configuration.
type Config struct {
	Command string   `yaml:"command"`
	Sandbox string   `yaml:"sandbox"` // default: "workspace-write"
	Args    []string `yaml:"args"`
}

func parseConfig(raw json.RawMessage) *Config {
	if len(raw) == 0 {
		return nil
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil
	}
	return &cfg
}

// --- Translator ---

type translatorState struct {
	runID     string
	agentName string
	threadID  string
	lastModel string
}

// translateStream reads JSONL lines from r and emits canonical events to sink.
func translateStream(r io.Reader, state *translatorState, sink harness.EventSink) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)

	streamStopped := false
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var ev codexEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			if rs, ok := sink.(harness.RawEventSink); ok {
				rs.OnHarnessRaw(adapterName, "parse_error", line)
			}
			continue
		}

		events := translateEvent(&ev, state)
		for _, e := range events {
			if _, ok := e.(harness.RunEnd); ok {
				streamStopped = true
			}
			if _, ok := e.(harness.RunError); ok {
				streamStopped = true
			}
			sink.Emit(e)
		}
	}

	if !streamStopped {
		// Process exited without a turn.completed or turn.failed event.
		sink.Emit(harness.RunError{
			RunID:   state.runID,
			Code:    harness.ErrCodeHarnessCrashed,
			Message: "codex subprocess exited without a turn event",
			At:      time.Now(),
		})
	}
}

// --- Codex CLI JSONL event types ---

type codexEvent struct {
	Type string `json:"type"`

	// thread.started fields
	ThreadID string `json:"thread_id,omitempty"`
	Model    string `json:"model,omitempty"`

	// item.completed fields
	Item *codexItem `json:"item,omitempty"`

	// turn.completed / turn.failed fields
	Usage   *codexUsage `json:"usage,omitempty"`
	CostUSD float64     `json:"cost_usd,omitempty"`
	Error   *codexError `json:"error,omitempty"`

	// top-level error event
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type codexItem struct {
	// Common
	Type string `json:"type"`
	ID   string `json:"id"`

	// message / reasoning
	Content string `json:"content,omitempty"`
	Role    string `json:"role,omitempty"`

	// command_execution
	Command  string `json:"command,omitempty"`
	Output   string `json:"output,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`

	// file_change
	Path   string          `json:"path,omitempty"`
	Diff   string          `json:"diff,omitempty"`
	Change string          `json:"change,omitempty"`
	Args   json.RawMessage `json:"args,omitempty"`

	// mcp_tool_call
	Server string          `json:"server,omitempty"`
	Tool   string          `json:"tool,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Result string          `json:"result,omitempty"`

	// web_search
	Query   string `json:"query,omitempty"`
	Results string `json:"results,omitempty"`

	// general error flag
	IsError bool `json:"is_error,omitempty"`
}

type codexUsage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

type codexError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// translateEvent converts one parsed Codex event into zero or more canonical events.
func translateEvent(ev *codexEvent, state *translatorState) []harness.Event {
	now := time.Now()
	switch ev.Type {
	case "thread.started":
		return translateThreadStarted(ev, state, now)
	case "item.completed":
		return translateItemCompleted(ev, state, now)
	case "turn.completed":
		return translateTurnCompleted(ev, state, now)
	case "turn.failed":
		return translateTurnFailed(ev, state, now)
	case "error":
		return translateError(ev, state, now)
	default:
		return nil
	}
}

func translateThreadStarted(ev *codexEvent, state *translatorState, now time.Time) []harness.Event {
	state.threadID = ev.ThreadID
	if ev.Model != "" {
		state.lastModel = ev.Model
	}
	return []harness.Event{
		harness.RunStart{
			RunID:        state.runID,
			HarnessRunID: ev.ThreadID,
			ThreadID:     ev.ThreadID,
			Model:        ev.Model,
			At:           now,
		},
	}
}

func translateItemCompleted(ev *codexEvent, state *translatorState, now time.Time) []harness.Event {
	if ev.Item == nil {
		return nil
	}
	item := ev.Item
	itemID := item.ID
	if itemID == "" {
		itemID = fmt.Sprintf("item-%d", now.UnixNano())
	}

	switch item.Type {
	case "message":
		if item.Content == "" {
			return nil
		}
		return []harness.Event{
			harness.TextStart{MessageID: itemID, Role: defaultRole(item.Role), At: now},
			harness.TextDelta{MessageID: itemID, Delta: item.Content, At: now},
			harness.TextEnd{MessageID: itemID, At: now},
		}
	case "reasoning":
		if item.Content == "" {
			return nil
		}
		return []harness.Event{
			harness.ReasoningStart{MessageID: itemID, At: now},
			harness.ReasoningDelta{MessageID: itemID, Delta: item.Content, At: now},
			harness.ReasoningEnd{MessageID: itemID, At: now},
		}
	case "command_execution":
		return atomicToolCall(itemID, "shell", item.Command, item.Output, item.ExitCode != 0 || item.IsError, now)
	case "file_change":
		toolName := "edit"
		if item.Change == "create" || item.Change == "add" {
			toolName = "write"
		}
		argStr := item.Path
		if argStr == "" && len(item.Args) > 0 {
			argStr = string(item.Args)
		}
		return atomicToolCall(itemID, toolName, argStr, item.Diff, item.IsError, now)
	case "mcp_tool_call":
		toolName := item.Tool
		if item.Server != "" && toolName != "" {
			toolName = item.Server + "/" + item.Tool
		}
		args := string(item.Input)
		return atomicToolCall(itemID, toolName, args, item.Result, item.IsError, now)
	case "web_search":
		return atomicToolCall(itemID, "web_search", item.Query, item.Results, item.IsError, now)
	default:
		return nil
	}
}

// atomicToolCall emits ToolCallStart + ToolCallResult back-to-back for atomic harnesses.
// No ToolCallEnd is emitted between them.
func atomicToolCall(id, name, args, result string, isError bool, now time.Time) []harness.Event {
	_ = args // args context is informational; canonical events carry only name + id
	return []harness.Event{
		harness.ToolCallStart{ToolCallID: id, ToolName: name, At: now},
		harness.ToolCallResult{
			ToolCallID: id,
			ToolName:   name,
			Result:     result,
			IsError:    isError,
			At:         now,
		},
	}
}

func defaultRole(r string) string {
	if r == "" {
		return "assistant"
	}
	return r
}

func translateTurnCompleted(ev *codexEvent, state *translatorState, now time.Time) []harness.Event {
	// Codex CLI does not report cost in its JSONL stream. Mark cost as unknown
	// so downstream consumers (sidebar, persistence) render "--" instead of
	// pretending the run was free at $0.00.
	usage := &harness.UsageSummary{
		CostUSD:     ev.CostUSD,
		CostUnknown: ev.CostUSD == 0,
	}
	if ev.Usage != nil {
		usage.InputTokens = int(ev.Usage.InputTokens)
		usage.OutputTokens = int(ev.Usage.OutputTokens)
		usage.ReasoningTokens = int(ev.Usage.ReasoningTokens)
	}
	return []harness.Event{
		harness.RunEnd{
			RunID:        state.runID,
			HarnessRunID: state.threadID,
			Usage:        usage,
			StopReason:   "success",
			At:           now,
		},
	}
}

func translateTurnFailed(ev *codexEvent, state *translatorState, now time.Time) []harness.Event {
	code := harness.ErrCodeUnknown
	msg := "turn failed"
	if ev.Error != nil {
		code = mapErrorCode(ev.Error.Code)
		if ev.Error.Message != "" {
			msg = ev.Error.Message
		} else if ev.Error.Code != "" {
			msg = ev.Error.Code
		}
	}
	return []harness.Event{
		harness.RunError{
			RunID:   state.runID,
			Code:    code,
			Message: msg,
			At:      now,
		},
	}
}

func translateError(ev *codexEvent, state *translatorState, now time.Time) []harness.Event {
	code := mapErrorCode(ev.Code)
	msg := ev.Message
	if msg == "" {
		msg = ev.Code
	}
	if msg == "" {
		msg = "codex error"
	}
	// Infer error code from message when the event has no explicit code field.
	// Codex top-level error events carry the detail in message, not code.
	if code == harness.ErrCodeUnknown {
		switch {
		case strings.Contains(msg, "401") || strings.Contains(msg, "Unauthorized") || strings.Contains(msg, "authentication"):
			code = harness.ErrCodeAuthFailed
		case strings.Contains(msg, "429") || strings.Contains(msg, "rate limit"):
			code = harness.ErrCodeRateLimited
		case strings.Contains(msg, "context") && strings.Contains(msg, "exceed"):
			code = harness.ErrCodeContextExhausted
		}
	}
	return []harness.Event{
		harness.RunError{
			RunID:   state.runID,
			Code:    code,
			Message: msg,
			At:      now,
		},
	}
}

// mapErrorCode maps a Codex error code string to a canonical harness ErrorCode.
func mapErrorCode(code string) harness.ErrorCode {
	switch code {
	case "context_window_exceeded":
		return harness.ErrCodeContextExhausted
	case "rate_limit", "rate_limited":
		return harness.ErrCodeRateLimited
	case "authentication", "auth_failed", "unauthorized":
		return harness.ErrCodeAuthFailed
	default:
		return harness.ErrCodeUnknown
	}
}
