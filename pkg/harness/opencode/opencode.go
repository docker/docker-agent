// Package opencode implements the OpenCode CLI harness adapter for docker-agent.
// It spawns `opencode run --format json` as a subprocess and translates its
// NDJSON event stream into canonical harness events.
//
// # Invocation
//
//	opencode run \
//	  --format json \
//	  --model <provider>/<model> \
//	  [--agent <agent>] \
//	  --dangerously-skip-permissions \
//	  [--session <id>] \
//	  -- <prompt>
//
// # Wire format
//
// OpenCode emits NDJSON on stdout with the following event types:
//
//	step_start    - opens a step (no canonical equivalent; absorbed)
//	text          - sealed assistant text (no streaming deltas)
//	reasoning     - sealed reasoning block
//	tool_use      - ATOMIC: state.input + state.output in one event
//	step_finish   - carries cost and token usage; emitted before RunEnd
//	error         - terminal error
//
// # Known gaps
//
// OpenCode CLI does not expose a per-call system prompt flag. When
// SubSessionRequest.SystemPrompt is set, the adapter prepends it to the task
// with a separator and logs a warning.
package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/docker/docker-agent/pkg/harness"
)

const adapterName = "opencode"

// Adapter implements harness.HarnessAdapter for the OpenCode CLI.
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
			SystemPrompt:  false, // KNOWN GAP: CLI has no --system-prompt
			Reasoning:     true,
			TextDeltas:    false,
			MultiTurn:     true,
			StreamingArgs: false,
		},
		BuiltInTools: []string{"bash", "write", "edit", "read", "glob", "grep"},
	}
}

// Run executes one sub-session against the OpenCode CLI.
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

	binary := "opencode"
	if cfg != nil && cfg.Command != "" {
		binary = cfg.Command
	}

	// Handle the system prompt gap: prepend to task with a warning.
	task := req.Task
	if req.SystemPrompt != "" {
		slog.Warn("opencode CLI does not support per-call system prompts; prepending to task",
			"agent", req.RunID)
		task = req.SystemPrompt + "\n\n---\n\n" + req.Task
	}

	args := buildArgs(req, cfg, task)

	cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec
	cmd.Dir = req.WorkingDir
	cmd.Env = buildEnv(req)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("opencode stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("opencode stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("opencode start: %w", err)
	}

	// Synthesize RunStart immediately -- OpenCode does not emit one.
	req.Events.Emit(harness.RunStart{
		RunID: req.RunID,
		At:    time.Now(),
	})

	// Drain stderr to debug log.
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			slog.Debug("opencode stderr", "line", scanner.Text())
		}
	}()

	// Read and translate NDJSON events from stdout.
	state := &translatorState{
		runID:     req.RunID,
		agentName: req.RunID,
	}
	streamStopped := translateStream(stdout, state, req.Events)

	waitErr := cmd.Wait()

	// If the stream ended without a terminal event, decide based on exit code.
	if !streamStopped {
		if waitErr != nil {
			req.Events.Emit(harness.RunError{
				RunID:   req.RunID,
				Code:    harness.ErrCodeHarnessCrashed,
				Message: fmt.Sprintf("opencode subprocess exited without a terminal event: %v", waitErr),
				At:      time.Now(),
			})
		} else {
			// Exit 0 with no step_finish: emit RunEnd with empty usage.
			req.Events.Emit(harness.RunEnd{
				RunID:      req.RunID,
				Usage:      &harness.UsageSummary{},
				StopReason: "end_turn",
				At:         time.Now(),
			})
		}
	}

	return nil
}

// buildArgs constructs the opencode CLI arguments for a sub-session.
func buildArgs(req harness.SubSessionRequest, cfg *Config, task string) []string {
	args := []string{
		"run",
		"--format", "json",
		"--dangerously-skip-permissions",
	}

	if cfg != nil {
		if cfg.Model != "" {
			args = append(args, "--model", cfg.Model)
		}
		if cfg.Agent != "" {
			args = append(args, "--agent", cfg.Agent)
		}
		args = append(args, cfg.Args...)
	}

	if req.ResumeToken != "" {
		args = append(args, "--session", req.ResumeToken)
	}

	// Use `--` to separate the prompt from flags.
	args = append(args, "--", task)
	return args
}

// buildEnv constructs the environment for the opencode subprocess.
func buildEnv(req harness.SubSessionRequest) []string {
	env := os.Environ()
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// --- Config ---

// Config holds OpenCode adapter-specific configuration.
type Config struct {
	Command string   `yaml:"command" json:"command"`
	Model   string   `yaml:"model" json:"model"`   // e.g. "anthropic/claude-sonnet-4-5"
	Agent   string   `yaml:"agent" json:"agent"`   // opencode agent name
	Args    []string `yaml:"args" json:"args"`
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
	sessionID string
}

// translateStream reads NDJSON lines from r and emits canonical events to sink.
// Returns true if a terminal event (RunEnd or RunError) was emitted from the stream.
func translateStream(r io.Reader, state *translatorState, sink harness.EventSink) bool {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)

	streamStopped := false
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var ev opencodeEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			if rs, ok := sink.(harness.RawEventSink); ok {
				rs.OnHarnessRaw(adapterName, "parse_error", line)
			}
			continue
		}

		events := translateEvent(&ev, state)
		for _, e := range events {
			switch e.(type) {
			case harness.RunEnd, harness.RunError:
				streamStopped = true
			}
			sink.Emit(e)
		}
	}

	return streamStopped
}

// --- OpenCode NDJSON event types ---

type opencodeEvent struct {
	Type  string          `json:"type"`
	Part  json.RawMessage `json:"part,omitempty"`
	Error *opencodeError  `json:"error,omitempty"`
}

type opencodeError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type opencodeStepStart struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
}

type opencodeStepFinish struct {
	Type   string          `json:"type"`
	Reason string          `json:"reason"`
	Cost   float64         `json:"cost"`
	Tokens opencodeTokens  `json:"tokens"`
}

type opencodeTokens struct {
	Input     int64               `json:"input"`
	Output    int64               `json:"output"`
	Reasoning int64               `json:"reasoning"`
	Cache     opencodeTokensCache `json:"cache"`
}

type opencodeTokensCache struct {
	Read  int64 `json:"read"`
	Write int64 `json:"write"`
}

type opencodeText struct {
	Type string         `json:"type"`
	Text string         `json:"text"`
	Time opencodeTimeRange `json:"time"`
}

type opencodeReasoning struct {
	Type string            `json:"type"`
	Text string            `json:"text"`
	Time opencodeTimeRange `json:"time"`
}

type opencodeToolUse struct {
	Type   string             `json:"type"`
	ID     string             `json:"id"`
	Tool   string             `json:"tool"`
	CallID string             `json:"callID"`
	State  opencodeToolState  `json:"state"`
}

type opencodeToolState struct {
	Status string            `json:"status"`
	Input  json.RawMessage   `json:"input"`
	Output string            `json:"output"`
	Title  string            `json:"title"`
	Error  string            `json:"error,omitempty"`
	Time   opencodeTimeRange `json:"time"`
}

type opencodeTimeRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// translateEvent converts one parsed OpenCode event into zero or more canonical events.
func translateEvent(ev *opencodeEvent, state *translatorState) []harness.Event {
	now := time.Now()
	switch ev.Type {
	case "step_start":
		return translateStepStart(ev, state, now)
	case "step_finish":
		return translateStepFinish(ev, state, now)
	case "text":
		return translateText(ev, state, now)
	case "reasoning":
		return translateReasoning(ev, state, now)
	case "tool_use":
		return translateToolUse(ev, state, now)
	case "error":
		return translateError(ev, state, now)
	default:
		return nil
	}
}

func translateStepStart(ev *opencodeEvent, state *translatorState, now time.Time) []harness.Event {
	// step_start has no canonical equivalent (RunStart is synthesized at process start).
	// Capture the session ID for completeness.
	if len(ev.Part) == 0 {
		return nil
	}
	var p opencodeStepStart
	if err := json.Unmarshal(ev.Part, &p); err == nil && p.SessionID != "" {
		state.sessionID = p.SessionID
	}
	return nil
}

func translateStepFinish(ev *opencodeEvent, state *translatorState, now time.Time) []harness.Event {
	if len(ev.Part) == 0 {
		return []harness.Event{
			harness.RunEnd{
				RunID:        state.runID,
				HarnessRunID: state.sessionID,
				Usage:        &harness.UsageSummary{},
				StopReason:   "end_turn",
				At:           now,
			},
		}
	}
	var p opencodeStepFinish
	if err := json.Unmarshal(ev.Part, &p); err != nil {
		return []harness.Event{
			harness.RunEnd{
				RunID:        state.runID,
				HarnessRunID: state.sessionID,
				Usage:        &harness.UsageSummary{},
				StopReason:   "end_turn",
				At:           now,
			},
		}
	}

	usage := &harness.UsageSummary{
		InputTokens:     int(p.Tokens.Input),
		OutputTokens:    int(p.Tokens.Output),
		ReasoningTokens: int(p.Tokens.Reasoning),
		CacheReadTokens: int(p.Tokens.Cache.Read),
		CacheCreationTokens: int(p.Tokens.Cache.Write),
		CostUSD:         p.Cost,
	}
	stop := p.Reason
	if stop == "" {
		stop = "end_turn"
	}
	return []harness.Event{
		harness.RunEnd{
			RunID:        state.runID,
			HarnessRunID: state.sessionID,
			Usage:        usage,
			StopReason:   stop,
			At:           now,
		},
	}
}

func translateText(ev *opencodeEvent, state *translatorState, now time.Time) []harness.Event {
	if len(ev.Part) == 0 {
		return nil
	}
	var p opencodeText
	if err := json.Unmarshal(ev.Part, &p); err != nil {
		return nil
	}
	if p.Text == "" {
		return nil
	}
	msgID := fmt.Sprintf("text-%d", now.UnixNano())
	return []harness.Event{
		harness.TextStart{MessageID: msgID, Role: "assistant", At: now},
		harness.TextDelta{MessageID: msgID, Delta: p.Text, At: now},
		harness.TextEnd{MessageID: msgID, At: now},
	}
}

func translateReasoning(ev *opencodeEvent, state *translatorState, now time.Time) []harness.Event {
	if len(ev.Part) == 0 {
		return nil
	}
	var p opencodeReasoning
	if err := json.Unmarshal(ev.Part, &p); err != nil {
		return nil
	}
	if p.Text == "" {
		return nil
	}
	msgID := fmt.Sprintf("reasoning-%d", now.UnixNano())
	return []harness.Event{
		harness.ReasoningStart{MessageID: msgID, At: now},
		harness.ReasoningDelta{MessageID: msgID, Delta: p.Text, At: now},
		harness.ReasoningEnd{MessageID: msgID, At: now},
	}
}

func translateToolUse(ev *opencodeEvent, state *translatorState, now time.Time) []harness.Event {
	if len(ev.Part) == 0 {
		return nil
	}
	var p opencodeToolUse
	if err := json.Unmarshal(ev.Part, &p); err != nil {
		return nil
	}

	// Only emit canonical events for terminal states (completed/error).
	// "running" or other intermediate states are absorbed.
	if p.State.Status != "completed" && p.State.Status != "error" {
		return nil
	}

	// Use the OpenCode call ID for traceability; fall back to id.
	toolCallID := p.CallID
	if toolCallID == "" {
		toolCallID = p.ID
	}

	isError := p.State.Status == "error"
	result := p.State.Output
	if isError && p.State.Error != "" {
		result = p.State.Error
	}

	return []harness.Event{
		harness.ToolCallStart{
			ToolCallID: toolCallID,
			ToolName:   p.Tool,
			At:         now,
		},
		harness.ToolCallResult{
			ToolCallID: toolCallID,
			ToolName:   p.Tool,
			Result:     result,
			IsError:    isError,
			At:         now,
		},
	}
}

func translateError(ev *opencodeEvent, state *translatorState, now time.Time) []harness.Event {
	code := harness.ErrCodeUnknown
	msg := "opencode error"
	if ev.Error != nil {
		msg = ev.Error.Message
		switch ev.Error.Type {
		case "context_length":
			code = harness.ErrCodeContextExhausted
		case "auth":
			code = harness.ErrCodeAuthFailed
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
