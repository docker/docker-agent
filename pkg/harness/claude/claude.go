// Package claude implements the Claude Code CLI harness adapter for docker-agent.
// It spawns `claude --print --output-format stream-json` as a subprocess and
// translates its NDJSON event stream into canonical harness events.
//
// # Invocation
//
//	claude \
//	  --print \
//	  --output-format stream-json \
//	  --verbose \
//	  --bare \
//	  --no-session-persistence \
//	  --permission-mode bypassPermissions \
//	  --dangerously-skip-permissions \
//	  --session-id <uuid> \
//	  --system-prompt-file <path> \
//	  --max-turns 50
//
// User messages are written to stdin as NDJSON SDKUserMessage records
// (--input-format stream-json). Multi-turn sessions keep the process alive
// and write subsequent messages to stdin.
//
// # Wire format
//
// Claude Code emits NDJSON on stdout. Each line is a JSON object with a
// "type" discriminator. See the Anthropic Claude Code SDK documentation for
// the full event catalog.
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/harness"
)

const adapterName = "claude-code"

// Adapter implements harness.HarnessAdapter for the Claude Code CLI.
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
			SystemPrompt:  true,
			Reasoning:     true,
			TextDeltas:    true,  // --include-partial-messages enables token streaming
			MultiTurn:     true,
			StreamingArgs: true,  // input_json_delta events stream tool args
		},
		BuiltInTools: []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep", "LS"},
	}
}

// Run executes one sub-session against the Claude Code CLI.
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
	binary := "claude"
	if cfg := parseConfig(req.Config); cfg != nil && cfg.Command != "" {
		binary = cfg.Command
	}

	args, cleanup := buildArgs(req)
	defer cleanup()

	cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec
	cmd.Dir = req.WorkingDir
	cmd.Env = buildEnv(req)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("claude stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("claude stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("claude stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("claude start: %w", err)
	}

	// Write the user message to stdin and close it (single-turn mode).
	go func() {
		defer stdin.Close()
		msg := map[string]any{
			"type": "user",
			"message": map[string]any{
				"role":    "user",
				"content": req.Task,
			},
		}
		data, _ := json.Marshal(msg)
		data = append(data, '\n')
		if _, err := stdin.Write(data); err != nil {
			slog.Debug("claude stdin write", "error", err)
		}
	}()

	// Drain stderr to debug log.
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			slog.Debug("claude stderr", "line", scanner.Text())
		}
	}()

	// Read and translate NDJSON events from stdout.
	state := &translatorState{
		runID:     req.RunID,
		agentName: req.RunID, // use RunID as agent name for sub-session events
		toolNames: make(map[string]string),
	}
	translateStream(stdout, state, req.Events)

	return cmd.Wait()
}

// buildArgs constructs the claude CLI arguments for a sub-session.
// Returns the args slice and a cleanup function that removes any temp files.
func buildArgs(req harness.SubSessionRequest) ([]string, func()) {
	cleanup := func() {}

	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--bare",
		"--no-session-persistence",
		"--input-format", "stream-json",
		"--include-partial-messages",
		"--max-turns", "50",
	}

	if req.ResumeToken != "" {
		args = append(args, "--resume", req.ResumeToken)
	} else if req.SystemPrompt != "" {
		// Write system prompt to a temp file to avoid shell-escaping issues.
		if f, err := writeTempPrompt(req.SystemPrompt); err == nil {
			args = append(args, "--system-prompt-file", f)
			cleanup = func() { os.Remove(f) } //nolint:errcheck
		}
	}

	cfg := parseConfig(req.Config)
	if cfg != nil {
		// Allow opt-out of partial messages streaming.
		if cfg.IncludePartialMessages != nil && !*cfg.IncludePartialMessages {
			for i, a := range args {
				if a == "--include-partial-messages" {
					args = append(args[:i], args[i+1:]...)
					break
				}
			}
		}
		args = append(args, cfg.Args...)
		if cfg.Model != "" {
			args = append(args, "--model", cfg.Model)
		}
		if cfg.MaxTurns > 0 {
			// Override the default --max-turns.
			for i, a := range args {
				if a == "--max-turns" && i+1 < len(args) {
					args[i+1] = fmt.Sprintf("%d", cfg.MaxTurns)
					break
				}
			}
		}
		// Honor permission policy from agent config.
		if cfg.PermissionMode != "" {
			args = append(args, "--permission-mode", cfg.PermissionMode)
			if cfg.PermissionMode == "bypassPermissions" {
				args = append(args, "--dangerously-skip-permissions")
			}
		}
	}

	return args, cleanup
}

// safeEnvKeys are environment variables passed through to harness subprocesses.
// This is an allowlist: only these keys are inherited from the parent process.
// Additional keys can be injected via SubSessionRequest.Env.
var safeEnvKeys = []string{
	// System
	"HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "TEMP", "TMP",
	"LANG", "LC_ALL", "LC_CTYPE", "TERM", "COLORTERM",
	"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
	// AI provider API keys (harnesses need these to authenticate)
	"ANTHROPIC_API_KEY",
	"OPENAI_API_KEY",
	"GEMINI_API_KEY", "GOOGLE_API_KEY",
	"GITHUB_TOKEN", "GH_TOKEN",
	// Node/npm (harnesses are typically npm-installed CLIs)
	"NODE_PATH", "NPM_CONFIG_PREFIX",
}

// buildEnv constructs the environment for the claude subprocess.
// Only safeEnvKeys are inherited from the parent process; all other parent
// env vars are dropped to prevent credential leakage to the subprocess.
// Additional vars can be injected via SubSessionRequest.Env.
func buildEnv(req harness.SubSessionRequest) []string {
	// Build allowlist from parent env.
	safe := make(map[string]bool, len(safeEnvKeys))
	for _, k := range safeEnvKeys {
		safe[k] = true
	}

	var env []string
	for _, kv := range os.Environ() {
		idx := strings.IndexByte(kv, '=')
		if idx < 0 {
			continue
		}
		k := kv[:idx]
		if safe[k] {
			env = append(env, kv)
		}
	}

	// Inject caller-specified env vars (these are explicitly opted-in).
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// writeTempPrompt writes the system prompt to a temp file and returns its path.
func writeTempPrompt(prompt string) (string, error) {
	f, err := os.CreateTemp("", "claude-prompt-*.txt")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(prompt); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// --- Config ---

// Config holds Claude Code adapter-specific configuration.
type Config struct {
	Command        string `yaml:"command"`
	Model          string `yaml:"model"`
	Args           []string `yaml:"args"`
	MaxTurns       int    `yaml:"max_turns"`
	// PermissionMode maps to Claude Code's --permission-mode flag.
	// Valid values: acceptEdits (default), bypassPermissions.
	// bypassPermissions requires i_understand_the_risk: true in the agent config.
	PermissionMode string `yaml:"permission_mode"`
	// IncludePartialMessages controls --include-partial-messages (default true).
	// Set to false to disable token streaming and revert to complete-message mode.
	IncludePartialMessages *bool `yaml:"include_partial_messages"`
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
	toolNames map[string]string // tool_use_id -> tool name
	lastModel string

	// Streaming state for --include-partial-messages.
	// streamingMsgID is the Anthropic message ID currently being streamed.
	streamingMsgID string
	// blockTypes maps content_block index -> block type ("text"|"thinking"|"tool_use")
	blockTypes map[int]string
	// blockToolID maps content_block index -> tool_use id (for tool_use blocks)
	blockToolID map[int]string
	// streamedBlocks maps msgID -> set of block indices already delivered via
	// stream_event, so translateAssistant can skip re-emitting them.
	streamedBlocks map[string]map[int]bool
}

// translateStream reads NDJSON lines from r and emits canonical events to sink.
func translateStream(r io.Reader, state *translatorState, sink harness.EventSink) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)

	streamStopped := false
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var ev claudeEvent
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
		// Process exited without a result event -- treat as crash.
		sink.Emit(harness.RunError{
			RunID:   state.runID,
			Code:    harness.ErrCodeHarnessCrashed,
			Message: "claude subprocess exited without a result event",
			At:      time.Now(),
		})
	}
}

// --- Claude Code NDJSON event types ---

type claudeEvent struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype,omitempty"`
	UUID    string          `json:"uuid,omitempty"`
	// system/init fields
	SessionID string        `json:"session_id,omitempty"`
	Model     string        `json:"model,omitempty"`
	Tools     []claudeTool  `json:"tools,omitempty"`
	// assistant/user message
	Message json.RawMessage `json:"message,omitempty"`
	// result fields
	Result       string       `json:"result,omitempty"`
	IsError      bool         `json:"is_error,omitempty"`
	Usage        *claudeUsage `json:"usage,omitempty"`
	TotalCostUSD float64      `json:"total_cost_usd,omitempty"`
	DurationMS   int64        `json:"duration_ms,omitempty"`
	Errors       []string     `json:"errors,omitempty"`
	// stream_event fields (--include-partial-messages)
	Event           json.RawMessage `json:"event,omitempty"`
	ParentToolUseID string          `json:"parent_tool_use_id,omitempty"`
}

// anthropicSSEEvent is the embedded Anthropic API streaming event inside a stream_event.
type anthropicSSEEvent struct {
	Type         string            `json:"type"`
	Index        int               `json:"index"`
	Message      *anthropicMsgInit `json:"message,omitempty"`
	ContentBlock *anthropicBlock   `json:"content_block,omitempty"`
	Delta        *anthropicDelta   `json:"delta,omitempty"`
}

type anthropicMsgInit struct {
	ID    string `json:"id"`
	Model string `json:"model"`
}

type anthropicBlock struct {
	Type     string `json:"type"`     // "text" | "thinking" | "tool_use"
	ID       string `json:"id"`       // tool_use id
	Name     string `json:"name"`     // tool name
}

type anthropicDelta struct {
	Type        string `json:"type"`         // "text_delta" | "input_json_delta" | "thinking_delta"
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	Thinking    string `json:"thinking"`
}

type claudeTool struct {
	Name string `json:"name"`
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

type claudeMessage struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Content []claudeContent `json:"content"`
}

type claudeContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// translateEvent converts one parsed Claude event into zero or more canonical events.
func translateEvent(ev *claudeEvent, state *translatorState) []harness.Event {
	now := time.Now()
	switch ev.Type {
	case "system":
		return translateSystem(ev, state, now)
	case "assistant":
		return translateAssistant(ev, state, now)
	case "user":
		return translateUser(ev, state, now)
	case "result":
		return translateResult(ev, state, now)
	case "stream_event":
		return translateStreamEvent(ev, state, now)
	default:
		return nil
	}
}

// translateStreamEvent handles the stream_event type emitted by
// --include-partial-messages. It unwraps the embedded Anthropic SSE event
// and emits canonical streaming events (TextDelta, ReasoningDelta,
// ToolCallStart, ToolCallArgsDelta, ToolCallEnd).
func translateStreamEvent(ev *claudeEvent, state *translatorState, now time.Time) []harness.Event {
	if len(ev.Event) == 0 {
		return nil
	}
	var inner anthropicSSEEvent
	if err := json.Unmarshal(ev.Event, &inner); err != nil {
		return nil
	}

	switch inner.Type {
	case "message_start":
		if inner.Message != nil && inner.Message.ID != "" {
			state.streamingMsgID = inner.Message.ID
			if state.blockTypes == nil {
				state.blockTypes = make(map[int]string)
				state.blockToolID = make(map[int]string)
				state.streamedBlocks = make(map[string]map[int]bool)
			}
			state.streamedBlocks[state.streamingMsgID] = make(map[int]bool)
		}
		return nil

	case "content_block_start":
		if inner.ContentBlock == nil {
			return nil
		}
		if state.blockTypes == nil {
			state.blockTypes = make(map[int]string)
			state.blockToolID = make(map[int]string)
		}
		state.blockTypes[inner.Index] = inner.ContentBlock.Type
		msgID := state.streamingMsgID
		switch inner.ContentBlock.Type {
		case "text":
			return []harness.Event{harness.TextStart{MessageID: msgID, Role: "assistant", At: now}}
		case "thinking":
			return []harness.Event{harness.ReasoningStart{MessageID: msgID, At: now}}
		case "tool_use":
			state.toolNames[inner.ContentBlock.ID] = inner.ContentBlock.Name
			state.blockToolID[inner.Index] = inner.ContentBlock.ID
			return []harness.Event{harness.ToolCallStart{
				ToolCallID: inner.ContentBlock.ID,
				ToolName:   inner.ContentBlock.Name,
				At:         now,
			}}
		}
		return nil

	case "content_block_delta":
		if inner.Delta == nil {
			return nil
		}
		msgID := state.streamingMsgID
		switch inner.Delta.Type {
		case "text_delta":
			if inner.Delta.Text == "" {
				return nil
			}
			return []harness.Event{harness.TextDelta{MessageID: msgID, Delta: inner.Delta.Text, At: now}}
		case "thinking_delta":
			if inner.Delta.Thinking == "" {
				return nil
			}
			return []harness.Event{harness.ReasoningDelta{MessageID: msgID, Delta: inner.Delta.Thinking, At: now}}
		case "input_json_delta":
			id := state.blockToolID[inner.Index]
			if id == "" || inner.Delta.PartialJSON == "" {
				return nil
			}
			return []harness.Event{harness.ToolCallArgsDelta{ToolCallID: id, Delta: inner.Delta.PartialJSON, At: now}}
		}
		return nil

	case "content_block_stop":
		msgID := state.streamingMsgID
		typ := state.blockTypes[inner.Index]
		// Mark block as streamed so translateAssistant skips re-emitting it.
		if state.streamedBlocks == nil {
			state.streamedBlocks = make(map[string]map[int]bool)
		}
		if state.streamedBlocks[msgID] == nil {
			state.streamedBlocks[msgID] = make(map[int]bool)
		}
		state.streamedBlocks[msgID][inner.Index] = true
		switch typ {
		case "text":
			return []harness.Event{harness.TextEnd{MessageID: msgID, At: now}}
		case "thinking":
			return []harness.Event{harness.ReasoningEnd{MessageID: msgID, At: now}}
		case "tool_use":
			id := state.blockToolID[inner.Index]
			if id == "" {
				return nil
			}
			return []harness.Event{harness.ToolCallEnd{ToolCallID: id, At: now}}
		}
		return nil

	case "message_delta", "message_stop", "ping":
		return nil
	}
	return nil
}

func translateSystem(ev *claudeEvent, state *translatorState, now time.Time) []harness.Event {
	if ev.Subtype != "init" {
		return nil
	}
	if ev.Model != "" {
		state.lastModel = ev.Model
	}
	sessionID := ev.SessionID
	if sessionID == "" {
		sessionID = state.runID
	}
	return []harness.Event{
		harness.RunStart{
			RunID:        state.runID,
			HarnessRunID: sessionID,
			Model:        ev.Model,
			At:           now,
		},
	}
}

func translateAssistant(ev *claudeEvent, state *translatorState, now time.Time) []harness.Event {
	if len(ev.Message) == 0 {
		return nil
	}
	var msg claudeMessage
	if err := json.Unmarshal(ev.Message, &msg); err != nil {
		return nil
	}
	if msg.Model != "" {
		state.lastModel = msg.Model
	}

	var events []harness.Event
	msgID := msg.ID
	if msgID == "" {
		msgID = fmt.Sprintf("msg-%d", now.UnixNano())
	}

	for _, c := range msg.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				events = append(events,
					harness.TextStart{MessageID: msgID, Role: "assistant", At: now},
					harness.TextDelta{MessageID: msgID, Delta: c.Text, At: now},
					harness.TextEnd{MessageID: msgID, At: now},
				)
			}
		case "thinking":
			if c.Thinking != "" {
				events = append(events,
					harness.ReasoningStart{MessageID: msgID, At: now},
					harness.ReasoningDelta{MessageID: msgID, Delta: c.Thinking, At: now},
					harness.ReasoningEnd{MessageID: msgID, At: now},
				)
			}
		case "tool_use":
			state.toolNames[c.ID] = c.Name
			args := "{}"
			if len(c.Input) > 0 {
				args = string(c.Input)
			}
			events = append(events,
				harness.ToolCallStart{ToolCallID: c.ID, ToolName: c.Name, Args: args, At: now},
				harness.ToolCallEnd{ToolCallID: c.ID, At: now},
			)
		}
	}
	return events
}

func translateUser(ev *claudeEvent, state *translatorState, now time.Time) []harness.Event {
	if len(ev.Message) == 0 {
		return nil
	}
	var msg claudeMessage
	if err := json.Unmarshal(ev.Message, &msg); err != nil {
		return nil
	}

	var events []harness.Event
	for _, c := range msg.Content {
		if c.Type != "tool_result" {
			continue
		}
		toolName := state.toolNames[c.ToolUseID]
		events = append(events, harness.ToolCallResult{
			ToolCallID: c.ToolUseID,
			ToolName:   toolName,
			Result:     c.Content,
			IsError:    c.IsError,
			At:         now,
		})
	}
	return events
}

func translateResult(ev *claudeEvent, state *translatorState, now time.Time) []harness.Event {
	switch ev.Subtype {
	case "success":
		usage := &harness.UsageSummary{
			CostUSD:    ev.TotalCostUSD,
			DurationMS: ev.DurationMS,
		}
		if ev.Usage != nil {
			usage.InputTokens = int(ev.Usage.InputTokens)
			usage.OutputTokens = int(ev.Usage.OutputTokens)
			usage.CacheCreationTokens = int(ev.Usage.CacheCreationInputTokens)
			usage.CacheReadTokens = int(ev.Usage.CacheReadInputTokens)
		}
		return []harness.Event{
			harness.RunEnd{
				RunID:      state.runID,
				Usage:      usage,
				StopReason: "success",
				At:         now,
			},
		}
	case "error_max_turns":
		return []harness.Event{
			harness.RunError{
				RunID:   state.runID,
				Code:    harness.ErrCodeContextExhausted,
				Message: "max turns reached",
				At:      now,
			},
		}
	default:
		msg := ev.Result
		if len(ev.Errors) > 0 {
			msg = ev.Errors[0]
		}
		code := harness.ErrCodeUnknown
		if ev.Subtype == "error_max_budget_usd" {
			code = harness.ErrCodeRateLimited
		}
		return []harness.Event{
			harness.RunError{
				RunID:   state.runID,
				Code:    code,
				Message: fmt.Sprintf("%s: %s", ev.Subtype, msg),
				At:      now,
			},
		}
	}
}

// tempPromptDir returns the directory for temp system prompt files.
func tempPromptDir() string {
	return filepath.Join(os.TempDir(), "docker-agent-harness")
}
