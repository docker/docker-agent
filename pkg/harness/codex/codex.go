// Package codex implements the [github.com/rumpl/harness.Provider] interface
// for the OpenAI Codex CLI, plus a docker-agent-specific [Adapter.RunStreaming]
// entry point that spawns `codex exec` as a subprocess and streams parsed
// events back to a callback.
//
// # Invocation (print mode)
//
//	codex exec --json --dangerously-bypass-approvals-and-sandbox -- <prompt>
//
// # Invocation (RunStreaming)
//
// RunStreaming spawns `codex exec --json --dangerously-bypass-approvals-and-sandbox
// --skip-git-repo-check -C <workdir> -- <task>` for a fresh run, or
// `codex exec resume <token> --json -- <task>` when resuming. It reads the
// JSONL stream from stdout, calls fn for each canonical event, and returns
// a [harness.RunResult] populated with the thread ID, usage, final text, and
// any terminal error.
package codex

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"

	extharness "github.com/rumpl/harness"

	"github.com/docker/docker-agent/pkg/harness"
)

const adapterName = "codex"

// Adapter is the Codex provider. It implements
// [github.com/rumpl/harness.Provider] and adds [Adapter.RunStreaming] for
// docker-agent's sub-session orchestrator.
type Adapter struct{}

// New constructs a Codex [Adapter].
func New() *Adapter { return &Adapter{} }

func init() {
	harness.Register(&Adapter{})
}

// Name implements [extharness.Provider].
func (a *Adapter) Name() string { return adapterName }

// PrintCommand implements [extharness.Provider].
func (a *Adapter) PrintCommand(prompt string) string {
	return "codex exec --json --dangerously-bypass-approvals-and-sandbox -- " + extharness.ShellEscape(prompt)
}

// InteractiveArgs implements [extharness.Provider].
func (a *Adapter) InteractiveArgs(_ string) []string {
	return []string{"codex"}
}

// ParseStreamLine implements [extharness.Provider]. It is stateless: the
// thread_id captured from thread.started is only meaningful during a live
// streaming run, so stateless callers do not receive a synthetic event for it.
func (a *Adapter) ParseStreamLine(line string) []harness.Event {
	return parseStreamLine(line)
}

// --- RunStreaming ---

// RunStreaming spawns `codex exec` as a subprocess, reads JSONL from stdout,
// and invokes fn for each canonical event. It returns when the subprocess
// exits or ctx is cancelled. The returned [harness.RunResult] carries the
// thread ID (HarnessRunID) so callers can resume the session by setting
// SubSessionRequest.ResumeToken on a subsequent call.
func (a *Adapter) RunStreaming(ctx context.Context, req harness.SubSessionRequest, fn func(harness.Event)) harness.RunResult {
	if fn == nil {
		fn = func(harness.Event) {}
	}

	args := buildRunArgs(req)

	cmd := exec.CommandContext(ctx, "codex", args...) //nolint:gosec
	cmd.Dir = req.WorkingDir
	cmd.Env = buildEnv(req)
	// Put the harness subprocess (and any bash/tool children it spawns) in
	// its own process group so they cannot interact with docker-agent's
	// controlling terminal and corrupt the TUI state.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return harness.RunResult{Err: fmt.Errorf("codex stdout pipe: %w", err), ErrCode: harness.ErrCodeHarnessCrashed}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return harness.RunResult{Err: fmt.Errorf("codex stderr pipe: %w", err), ErrCode: harness.ErrCodeHarnessCrashed}
	}

	if err := cmd.Start(); err != nil {
		return harness.RunResult{Err: fmt.Errorf("codex start: %w", err), ErrCode: harness.ErrCodeHarnessCrashed}
	}

	// Drain stderr into slog.Debug.
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 256*1024), 1024*1024)
		for scanner.Scan() {
			slog.Debug("codex stderr", "line", scanner.Text())
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)

	result := harness.RunResult{}
	sawResult := false
	var streamErr error // captured from "error" event lines

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		// Snoop thread_id and error events directly from the raw object
		// before delegating to the typed parser. ParseStreamLine is
		// stateless and does not surface these on its own.
		if obj, ok := extharness.ParseJSON(line); ok {
			typ, _ := obj["type"].(string)
			switch typ {
			case "thread.started":
				if id, _ := obj["thread_id"].(string); id != "" && result.HarnessRunID == "" {
					result.HarnessRunID = id
				}
			case "error":
				msg, _ := obj["message"].(string)
				code, _ := obj["code"].(string)
				if msg == "" {
					msg = code
				}
				if msg == "" {
					msg = "codex error"
				}
				streamErr = newCodexStreamError(code, msg)
			case "turn.failed":
				if e, ok := obj["error"].(map[string]any); ok {
					code, _ := e["code"].(string)
					msg, _ := e["message"].(string)
					if msg == "" {
						msg = code
					}
					if msg == "" {
						msg = "codex turn failed"
					}
					streamErr = newCodexStreamError(code, msg)
				} else {
					streamErr = errors.New("codex turn failed")
				}
			}
		}

		for _, ev := range parseStreamLine(line) {
			fn(ev)
			switch ev.Type {
			case extharness.EventText:
				if ev.Text != "" {
					result.FinalText = ev.Text
				}
			case extharness.EventResult:
				sawResult = true
				if ev.Result != "" {
					result.FinalText = ev.Result
				}
				if ev.Usage != nil {
					result.Usage = ev.Usage
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Debug("codex stdout scan error", "error", err)
	}

	waitErr := cmd.Wait()
	<-stderrDone

	if streamErr != nil {
		result.Err = streamErr
		result.ErrCode = classifyErrorMessage(streamErr.Error())
		return result
	}
	if waitErr != nil {
		result.Err = fmt.Errorf("codex exited: %w", waitErr)
		result.ErrCode = classifyExitError(waitErr, ctx)
		return result
	}
	if !sawResult {
		result.Err = errors.New("codex subprocess exited without a turn.completed event")
		result.ErrCode = harness.ErrCodeHarnessCrashed
	}
	return result
}

// buildRunArgs constructs the `codex` arguments for a sub-session run. When
// req.ResumeToken is non-empty the run resumes an existing thread; otherwise
// it starts a fresh thread with the bypass flags. If req.SystemPrompt is set
// it is prepended to the task because codex exec has no --system-prompt flag.
func buildRunArgs(req harness.SubSessionRequest) []string {
	var args []string

	if req.ResumeToken != "" {
		args = append(args, "exec", "resume", req.ResumeToken, "--json")
	} else {
		args = append(args, "exec",
			"--json",
			"--dangerously-bypass-approvals-and-sandbox",
			"--skip-git-repo-check",
		)
		if req.WorkingDir != "" {
			args = append(args, "-C", req.WorkingDir)
		}
	}

	prompt := req.Task
	if req.ResumeToken == "" && req.SystemPrompt != "" {
		prompt = req.SystemPrompt + "\n\n" + req.Task
	}
	args = append(args, "--", prompt)
	return args
}

// codexStreamError is the error type produced from a Codex error/turn.failed
// event so callers can pattern-match on it if they need to.
type codexStreamError struct {
	Code    string
	Message string
}

func (e *codexStreamError) Error() string { return e.Message }

func newCodexStreamError(code, msg string) *codexStreamError {
	return &codexStreamError{Code: code, Message: msg}
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

// classifyErrorMessage infers an ErrorCode from a codex error message string.
// Codex top-level error events carry the detail in `message` rather than a
// machine-readable code, so we pattern-match common cases.
func classifyErrorMessage(msg string) harness.ErrorCode {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(msg, "401") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "authentication") ||
		strings.Contains(lower, "auth_failed"):
		return harness.ErrCodeAuthFailed
	case strings.Contains(msg, "429") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "rate_limit"):
		return harness.ErrCodeRateLimited
	case strings.Contains(lower, "context_window_exceeded") ||
		(strings.Contains(lower, "context") && strings.Contains(lower, "exceed")):
		return harness.ErrCodeContextExhausted
	}
	return harness.ErrCodeUnknown
}

// --- Stream parser ---

// parseStreamLine handles the Codex JSONL streaming format. It recognises:
//
//   - {"type":"thread.started", ...}                          -> no event
//   - {"type":"item.completed","item":{"type":"agent_message","text":"..."}}
//     -> EventText + EventResult
//   - {"type":"item.started","item":{"type":"command_execution","command":"..."}}
//     -> EventToolCall (Bash)
//   - {"type":"turn.completed","usage":{...}}                 -> EventResult (usage only)
//
// "error" and "turn.failed" events are not surfaced as canonical events here;
// they are captured by RunStreaming and threaded into RunResult.Err.
func parseStreamLine(line string) []harness.Event {
	obj, ok := extharness.ParseJSON(line)
	if !ok {
		return nil
	}
	typ, _ := obj["type"].(string)
	switch typ {
	case "item.completed":
		return parseItemCompleted(obj)
	case "item.started":
		return parseItemStarted(obj)
	case "turn.completed":
		return parseTurnCompleted(obj)
	}
	return nil
}

func parseItemCompleted(obj map[string]any) []harness.Event {
	item, ok := obj["item"].(map[string]any)
	if !ok {
		return nil
	}
	itemType, _ := item["type"].(string)
	if itemType != "agent_message" {
		return nil
	}
	text, _ := item["text"].(string)
	if text == "" {
		return nil
	}
	return []harness.Event{
		{Type: extharness.EventText, Text: text},
		{Type: extharness.EventResult, Result: text},
	}
}

func parseItemStarted(obj map[string]any) []harness.Event {
	item, ok := obj["item"].(map[string]any)
	if !ok {
		return nil
	}
	itemType, _ := item["type"].(string)
	if itemType != "command_execution" {
		return nil
	}
	command, _ := item["command"].(string)
	if command == "" {
		return nil
	}
	return []harness.Event{{
		Type:     extharness.EventToolCall,
		ToolName: "Bash",
		ToolArgs: command,
	}}
}

func parseTurnCompleted(obj map[string]any) []harness.Event {
	// Codex does not report cost in its JSONL stream; only token counts.
	usage := extharness.ExtractCodexUsage(obj)
	if usage == nil {
		return nil
	}
	return []harness.Event{{
		Type:  extharness.EventResult,
		Usage: usage,
	}}
}

// --- Env allowlist ---

// safeEnvKeys are environment variables passed through to the codex
// subprocess. Explicit allowlist; everything else from the parent env is
// dropped to prevent credential leakage. Additional vars can be injected
// via SubSessionRequest.Env.
var safeEnvKeys = []string{
	// System
	"HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "TEMP", "TMP",
	"LANG", "LC_ALL", "LC_CTYPE", "TERM", "COLORTERM",
	"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
	// AI provider credentials (codex authenticates against OpenAI by default)
	"ANTHROPIC_API_KEY",
	"OPENAI_API_KEY",
	"GEMINI_API_KEY", "GOOGLE_API_KEY",
	"GITHUB_TOKEN", "GH_TOKEN",
	// Node/npm (codex is an npm-installed CLI)
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

// Ensure compile-time conformance with the rumpl/harness Provider interface.
var _ extharness.Provider = (*Adapter)(nil)
