// Package claude implements the [github.com/rumpl/harness.Provider]
// interface for the Claude Code CLI, plus a docker-agent-specific
// [Adapter.RunStreaming] entry point that spawns `claude` as a subprocess and
// streams parsed events back to a callback.
//
// # Invocation (print mode)
//
//	claude --print --verbose --dangerously-skip-permissions \
//	    --output-format stream-json --include-partial-messages \
//	    --model <model> -p <prompt>
//
// # Invocation (RunStreaming)
//
// RunStreaming uses --input-format stream-json so user messages can be
// written to stdin as NDJSON, supports a system prompt via temp file, and
// honours ResumeToken via --resume. It emits text, tool_call, and result
// events to the supplied callback, deduping content blocks that arrive both
// as stream_event deltas and inside the final assistant message.
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"

	extharness "github.com/rumpl/harness"

	"github.com/docker/docker-agent/pkg/harness"
)

const adapterName = "claude-code"

// Effort mirrors [claudecode.Effort] for parity with the rumpl/harness
// reference implementation. The value is passed through as --effort.
type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortMax    Effort = "max"
)

// Adapter is the Claude Code provider. It implements
// [github.com/rumpl/harness.Provider] and adds [Adapter.RunStreaming] for
// docker-agent's sub-session orchestrator.
type Adapter struct {
	model  string
	effort Effort
}

// Option configures a Claude [Adapter].
type Option func(*Adapter)

// WithEffort sets the --effort flag.
func WithEffort(e Effort) Option {
	return func(a *Adapter) { a.effort = e }
}

// WithModel overrides the default model.
func WithModel(m string) Option {
	return func(a *Adapter) {
		if m != "" {
			a.model = m
		}
	}
}

// New constructs a Claude Code [Adapter] for the given model.
func New(model string, opts ...Option) *Adapter {
	a := &Adapter{model: model}
	for _, o := range opts {
		o(a)
	}
	return a
}

func init() {
	harness.Register(&Adapter{model: "claude-sonnet-4-5"})
}

// Name implements [extharness.Provider].
func (a *Adapter) Name() string { return adapterName }

// PrintCommand implements [extharness.Provider]. It mirrors the rumpl/harness
// claudecode provider and adds --include-partial-messages so callers can pick
// up partial text deltas if they want to.
func (a *Adapter) PrintCommand(prompt string) string {
	effortFlag := ""
	if a.effort != "" {
		effortFlag = fmt.Sprintf(" --effort %s", a.effort)
	}
	return fmt.Sprintf(
		"claude --print --verbose --dangerously-skip-permissions --output-format stream-json --include-partial-messages --model %s%s -p %s",
		extharness.ShellEscape(a.model),
		effortFlag,
		extharness.ShellEscape(prompt),
	)
}

// InteractiveArgs implements [extharness.Provider].
func (a *Adapter) InteractiveArgs(_ string) []string {
	args := []string{"claude", "--dangerously-skip-permissions", "--model", a.model}
	if a.effort != "" {
		args = append(args, "--effort", string(a.effort))
	}
	return args
}

// ParseStreamLine implements [extharness.Provider]. It is stateless: dedupe
// against stream_event content blocks is only meaningful within a live
// streaming session, and stateless callers receive both the deltas and the
// final assistant message exactly as the wire format delivers them.
func (a *Adapter) ParseStreamLine(line string) []harness.Event {
	return parseStreamLine(line, nil)
}

// --- RunStreaming ---

// RunStreaming spawns `claude` as a subprocess, pipes the user message in via
// stdin (NDJSON), parses NDJSON events from stdout, and invokes fn for each
// canonical event. It returns when the subprocess exits or ctx is cancelled.
//
// When req.ResumeToken is set the subprocess is started with --resume;
// otherwise req.SystemPrompt is written to a temp file and passed via
// --system-prompt-file. The streaming translator dedupes content blocks that
// appear both as stream_event deltas and inside the final assistant message
// so callers see each block exactly once.
func (a *Adapter) RunStreaming(ctx context.Context, req harness.SubSessionRequest, fn func(harness.Event)) harness.RunResult {
	if fn == nil {
		fn = func(harness.Event) {}
	}

	args, cleanup, err := a.buildRunArgs(req)
	if err != nil {
		return harness.RunResult{Err: err, ErrCode: harness.ErrCodeHarnessCrashed}
	}
	defer cleanup()

	cmd := exec.CommandContext(ctx, "claude", args...) //nolint:gosec
	cmd.Dir = req.WorkingDir
	cmd.Env = buildEnv(req)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return harness.RunResult{Err: fmt.Errorf("claude stdin pipe: %w", err), ErrCode: harness.ErrCodeHarnessCrashed}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return harness.RunResult{Err: fmt.Errorf("claude stdout pipe: %w", err), ErrCode: harness.ErrCodeHarnessCrashed}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return harness.RunResult{Err: fmt.Errorf("claude stderr pipe: %w", err), ErrCode: harness.ErrCodeHarnessCrashed}
	}

	if err := cmd.Start(); err != nil {
		return harness.RunResult{Err: fmt.Errorf("claude start: %w", err), ErrCode: harness.ErrCodeHarnessCrashed}
	}

	// Write user message to stdin then close so claude knows the turn is
	// complete. Multi-turn is handled by re-spawning with --resume.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
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

	// Drain stderr into slog.Debug.
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 256*1024), 1024*1024)
		for scanner.Scan() {
			slog.Debug("claude stderr", "line", scanner.Text())
		}
	}()

	// Read stdout NDJSON, translate to canonical events, accumulate result.
	state := &translatorState{
		toolNames:      make(map[string]string),
		blockTypes:     make(map[int]string),
		blockToolID:    make(map[int]string),
		streamedBlocks: make(map[string]map[int]bool),
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)

	result := harness.RunResult{}
	sawResult := false

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		// Capture HarnessRunID from the system/init event for session
		// resumption. We do this by snooping the raw line so the typed
		// Event vocabulary stays consistent with rumpl/harness.
		if id, ok := extractSessionID(line); ok && result.HarnessRunID == "" {
			result.HarnessRunID = id
		}
		for _, ev := range parseStreamLine(line, state) {
			fn(ev)
			if ev.Type == extharness.EventResult {
				sawResult = true
				if result.FinalText == "" && ev.Result != "" {
					result.FinalText = ev.Result
				}
				if ev.Usage != nil {
					result.Usage = ev.Usage
				}
			}
			if ev.Type == extharness.EventText && ev.Text != "" {
				// Track running text in case the result event omits it.
				if !sawResult {
					result.FinalText = ev.Text
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Debug("claude stdout scan error", "error", err)
	}

	waitErr := cmd.Wait()
	wg.Wait()

	if waitErr != nil {
		// Preserve any result we already captured; just annotate the
		// terminal error.
		result.Err = fmt.Errorf("claude exited: %w", waitErr)
		result.ErrCode = classifyExitError(waitErr, ctx)
		return result
	}
	if !sawResult {
		result.Err = errors.New("claude subprocess exited without a result event")
		result.ErrCode = harness.ErrCodeHarnessCrashed
	}
	return result
}

// buildRunArgs assembles the CLI arguments for a RunStreaming invocation and
// returns a cleanup func that removes any temp files.
func (a *Adapter) buildRunArgs(req harness.SubSessionRequest) ([]string, func(), error) {
	args := []string{
		"--print",
		"--verbose",
		"--dangerously-skip-permissions",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--include-partial-messages",
		"--model", a.model,
	}
	if a.effort != "" {
		args = append(args, "--effort", string(a.effort))
	}

	cleanup := func() {}

	if req.ResumeToken != "" {
		args = append(args, "--resume", req.ResumeToken)
	} else if req.SystemPrompt != "" {
		f, err := writeTempPrompt(req.SystemPrompt)
		if err != nil {
			return nil, cleanup, fmt.Errorf("write system prompt: %w", err)
		}
		args = append(args, "--system-prompt-file", f)
		cleanup = func() { _ = os.Remove(f) }
	}

	return args, cleanup, nil
}

// classifyExitError maps subprocess failures onto the canonical ErrorCode
// vocabulary. Context cancellation wins over signal/exit codes.
func classifyExitError(err error, ctx context.Context) harness.ErrorCode {
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return harness.ErrCodeHarnessTimeout
		}
		return harness.ErrCodeUserCanceled
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return harness.ErrCodeHarnessCrashed
	}
	return harness.ErrCodeUnknown
}

// --- Stream parser ---

// translatorState carries cross-line state for the streaming dedupe used by
// --include-partial-messages. Each Anthropic message ID maps to the set of
// block indices already delivered via stream_event content_block_start so the
// final `assistant` event that mirrors the same content can be filtered.
type translatorState struct {
	streamingMsgID string
	blockTypes     map[int]string // content_block index -> "text"|"thinking"|"tool_use"
	blockToolID    map[int]string // content_block index -> tool_use id
	toolNames      map[string]string
	streamedBlocks map[string]map[int]bool
}

// parseStreamLine parses one NDJSON line emitted by `claude --output-format
// stream-json`. When state is non-nil the parser dedupes content blocks that
// appeared as stream_event deltas; when state is nil every block in the
// assistant message is emitted as-is (stateless mode for ParseStreamLine).
func parseStreamLine(line string, state *translatorState) []harness.Event {
	obj, ok := extharness.ParseJSON(line)
	if !ok {
		return nil
	}
	typ, _ := obj["type"].(string)
	switch typ {
	case "stream_event":
		if state == nil {
			return nil
		}
		return parseStreamEvent(obj, state)
	case "assistant":
		return parseAssistant(obj, state)
	case "result":
		return parseResult(obj)
	}
	return nil
}

// parseStreamEvent translates a `--include-partial-messages` stream_event
// into canonical events. Text deltas become EventText events; tool_use blocks
// are emitted as EventToolCall on content_block_start using whatever
// argFields are populated. Stateful: marks blocks as already-delivered so a
// later `assistant` event can skip them.
func parseStreamEvent(obj map[string]any, state *translatorState) []harness.Event {
	inner, ok := obj["event"].(map[string]any)
	if !ok {
		return nil
	}
	innerType, _ := inner["type"].(string)

	switch innerType {
	case "message_start":
		msg, ok := inner["message"].(map[string]any)
		if !ok {
			return nil
		}
		if id, _ := msg["id"].(string); id != "" {
			state.streamingMsgID = id
			if state.streamedBlocks == nil {
				state.streamedBlocks = make(map[string]map[int]bool)
			}
			state.streamedBlocks[id] = make(map[int]bool)
		}
		return nil

	case "content_block_start":
		idx := intField(inner, "index")
		block, ok := inner["content_block"].(map[string]any)
		if !ok {
			return nil
		}
		blockType, _ := block["type"].(string)
		state.blockTypes[idx] = blockType

		// Mark this block as streamed so the corresponding `assistant`
		// event can skip re-emitting it. We mark on _start because Claude
		// Code can interleave assistant events mid-stream.
		if msgID := state.streamingMsgID; msgID != "" {
			if state.streamedBlocks[msgID] == nil {
				state.streamedBlocks[msgID] = make(map[int]bool)
			}
			state.streamedBlocks[msgID][idx] = true
		}

		if blockType == "tool_use" {
			toolID, _ := block["id"].(string)
			toolName, _ := block["name"].(string)
			if toolID != "" {
				state.blockToolID[idx] = toolID
				if toolName != "" {
					state.toolNames[toolID] = toolName
				}
			}
			// Defer emitting EventToolCall until content_block_stop when
			// the args are fully buffered; we don't have them yet and
			// rumpl/harness models tool_call as a single event.
		}
		return nil

	case "content_block_delta":
		delta, ok := inner["delta"].(map[string]any)
		if !ok {
			return nil
		}
		dtype, _ := delta["type"].(string)
		switch dtype {
		case "text_delta":
			text, _ := delta["text"].(string)
			if text == "" {
				return nil
			}
			return []harness.Event{{Type: extharness.EventText, Text: text}}
		}
		return nil

	case "content_block_stop":
		// Currently no canonical event is emitted on stop; tool_use is
		// surfaced via the final assistant message because that's the
		// only place args are guaranteed to be fully assembled.
		return nil
	}
	return nil
}

// parseAssistant translates a final `assistant` event from the wire format.
// When state is non-nil it skips blocks that were already streamed via
// stream_event deltas (so callers do not see "hello" twice).
func parseAssistant(obj map[string]any, state *translatorState) []harness.Event {
	msg, ok := obj["message"].(map[string]any)
	if !ok {
		return nil
	}
	content, ok := msg["content"].([]any)
	if !ok {
		return nil
	}
	msgID, _ := msg["id"].(string)

	var streamed map[int]bool
	if state != nil && msgID != "" {
		streamed = state.streamedBlocks[msgID]
		delete(state.streamedBlocks, msgID)
	}

	var events []harness.Event
	var texts []string

	flush := func() {
		if len(texts) > 0 {
			events = append(events, harness.Event{Type: extharness.EventText, Text: joinStrings(texts)})
			texts = texts[:0]
		}
	}

	for i, raw := range content {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		blockType, _ := block["type"].(string)
		// Always record tool names for downstream use even if streamed.
		if blockType == "tool_use" && state != nil {
			if id, _ := block["id"].(string); id != "" {
				if name, _ := block["name"].(string); name != "" {
					state.toolNames[id] = name
				}
			}
		}
		if streamed != nil && streamed[i] {
			// Block already emitted via stream_event deltas. For tool_use
			// we still need to surface the EventToolCall here because the
			// stream_event path defers it; the args are only complete in
			// the final assistant message.
			if blockType != "tool_use" {
				continue
			}
		}

		switch blockType {
		case "text":
			if t, _ := block["text"].(string); t != "" {
				texts = append(texts, t)
			}
		case "tool_use":
			name, _ := block["name"].(string)
			if name == "" {
				continue
			}
			argField, ok := extharness.ToolArgFields[name]
			if !ok {
				continue
			}
			input, ok := block["input"].(map[string]any)
			if !ok {
				continue
			}
			argValue, ok := input[argField].(string)
			if !ok {
				continue
			}
			flush()
			events = append(events, harness.Event{
				Type:     extharness.EventToolCall,
				ToolName: name,
				ToolArgs: argValue,
			})
		}
	}
	flush()
	return events
}

// parseResult translates a terminal `result` event into a single EventResult.
func parseResult(obj map[string]any) []harness.Event {
	result, _ := obj["result"].(string)
	return []harness.Event{{
		Type:   extharness.EventResult,
		Result: result,
		Usage:  extharness.ExtractUsage(obj),
	}}
}

// extractSessionID pulls session_id out of a raw NDJSON line for HarnessRunID
// tracking. Returns ("", false) if absent.
func extractSessionID(line string) (string, bool) {
	if !strings.Contains(line, `"session_id"`) {
		return "", false
	}
	obj, ok := extharness.ParseJSON(line)
	if !ok {
		return "", false
	}
	id, ok := obj["session_id"].(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// joinStrings is a tiny helper to avoid the strings.Builder overhead on the
// hot path. Equivalent to strings.Join(ss, "") without an allocation when
// len(ss)==1.
func joinStrings(ss []string) string {
	if len(ss) == 1 {
		return ss[0]
	}
	n := 0
	for _, s := range ss {
		n += len(s)
	}
	var b strings.Builder
	b.Grow(n)
	for _, s := range ss {
		b.WriteString(s)
	}
	return b.String()
}

func intField(m map[string]any, key string) int {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0
		}
		return int(i)
	}
	return 0
}

// --- Env allowlist ---

// safeEnvKeys are environment variables passed through to the claude
// subprocess. This is an explicit allowlist; everything else from the
// parent env is dropped to prevent credential leakage. Additional vars
// can be injected via SubSessionRequest.Env.
var safeEnvKeys = []string{
	// System
	"HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "TEMP", "TMP",
	"LANG", "LC_ALL", "LC_CTYPE", "TERM", "COLORTERM",
	"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
	// AI provider credentials
	"ANTHROPIC_API_KEY",
	"OPENAI_API_KEY",
	"GEMINI_API_KEY", "GOOGLE_API_KEY",
	"GITHUB_TOKEN", "GH_TOKEN",
	// Node/npm (claude is an npm-installed CLI)
	"NODE_PATH", "NPM_CONFIG_PREFIX",
}

func buildEnv(req harness.SubSessionRequest) []string {
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
		if safe[kv[:idx]] {
			env = append(env, kv)
		}
	}
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// writeTempPrompt writes prompt to a temp file and returns its path.
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

// Ensure compile-time conformance with the rumpl/harness Provider interface.
var _ extharness.Provider = (*Adapter)(nil)
