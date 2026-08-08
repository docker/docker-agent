package builtins

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/docker/docker-agent/pkg/hooks"
)

// ElideRepeatedToolResults is the registered name of the builtin
// tool_response_transform hook that stops re-sending a read-only tool's output
// when it is byte-for-byte identical to what the model already saw earlier in
// the same session.
//
// # Why this cannot serve stale data
//
// This is deliberately NOT a cache. The tool always executes and its fresh
// output is always what gets hashed; the hook only decides whether to repeat
// bytes the model has already been shown. There is no stored payload to go
// stale, no expiry to tune, and no invalidation to get wrong: if the file (or
// whatever the tool reads) changed by even one byte, the hashes differ and the
// full new output is passed through untouched.
//
// The saving is in tokens, not in I/O — a repeated read_file result becomes a
// one-line marker. Latency is unchanged because the tool still runs.
//
// # Effective range
//
// [LimitLargeToolResults] is auto-injected at the FRONT of
// tool_response_transform and the executor applies the first non-nil rewrite in
// config order, so whenever that builtin fires (results over ~50 KiB or 2000
// lines) its truncation wins and an elision marker would be discarded. Nothing
// a user writes in YAML can precede an auto-injected entry, so this builtin
// deliberately declines to act on payloads that large: eliding them is not
// possible today, and recording a fingerprint for them would only waste memory.
//
// The effective window is therefore (len(marker), maxToolCallResultBytes).
// Repeats above it stay bounded by limit_large_tool_results, which caps a single
// result but not the cost of repeating it.
const ElideRepeatedToolResults = "elide_repeated_tool_results"

// elidableCategories lists the tool categories this builtin will act on.
//
// ReadOnlyHint alone is not a purity signal in this codebase: several built-in
// tools set it for approval-gating reasons while still having effects
// (create_todo, update_todos, handoff), and for MCP tools it is copied verbatim
// from the remote server — so a third-party server could self-declare it and
// have its repeated output suppressed from the transcript and the persisted
// session. Pairing the hint with a category the agent's own toolsets own keeps
// that decision local, the same way limit_large_tool_results scopes itself.
var elidableCategories = map[string]bool{
	"filesystem": true,
	"lsp":        true,
	"rag":        true,
	"memory":     true,
	"git":        true,
}

const (
	// maxElideKeysPerSession bounds per-session memory. A session that calls
	// read-only tools with thousands of distinct argument sets stops recording
	// new fingerprints rather than growing without limit; already-recorded keys
	// keep working. Each entry is a 32-byte hash plus a map key.
	maxElideKeysPerSession = 4096

	// maxElideSessions bounds how many sessions are tracked at once. Cleanup is
	// driven by a session_end entry the operator has to wire up, and a session
	// that ends abnormally never fires it, so a long-lived `serve api` process
	// would otherwise accumulate one map per session it ever saw. Past the cap
	// the oldest tracked session is dropped; it simply stops eliding.
	maxElideSessions = 256
)

// elideState remembers, per session, the fingerprint of the most recent output
// seen for each (tool, arguments) pair.
//
// Package-level state because builtins are registered as plain functions and
// have nowhere else to live. Entries are dropped on session_end, on compaction,
// and on session_start; the session count is capped for the cases where none of
// those fire.
type elideState struct {
	mu sync.Mutex
	// seen maps session ID -> call key -> sha256 of the last output.
	seen map[string]map[string][sha256.Size]byte
	// order records session IDs in first-seen order so the oldest can be
	// dropped when the cap is reached.
	order []string
}

var elideStore = &elideState{seen: make(map[string]map[string][sha256.Size]byte)}

// elideRepeatedToolResults is the [hooks.BuiltinFunc] registered under
// [ElideRepeatedToolResults]. It dispatches on the event so one YAML entry can
// cover both the transform leg and the session_end cleanup.
func elideRepeatedToolResults(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
	if in == nil {
		return nil, nil
	}
	switch in.HookEventName {
	case hooks.EventToolResponseTransform:
		return elideRepeatedToolResponse(in), nil
	case hooks.EventSessionEnd:
		elideStore.forget(in.SessionID)
		return nil, nil
	case hooks.EventAfterCompaction:
		// Compaction drops the messages before the kept-tail boundary, so a
		// fingerprint can outlive the payload it stands for. Keeping it would
		// tell the model "nothing has changed" about bytes it can no longer
		// see, permanently — only a byte change would ever release the payload
		// again. Forgetting is the conservative direction: at worst one full
		// result is re-sent.
		elideStore.forget(in.SessionID)
		return nil, nil
	case hooks.EventSessionStart:
		// "compact" and "clear" rebuild the context the same way; "startup" and
		// "resume" begin one, where stale state from a previous process would
		// be equally wrong.
		elideStore.forget(in.SessionID)
		return nil, nil
	default:
		// Lenient on misconfiguration, matching redact_secrets: log the typo
		// but never fail the run loop over a misplaced hook entry.
		slog.Warn("elide_repeated_tool_results builtin invoked under unsupported event; no-op",
			"event", in.HookEventName)
		return nil, nil
	}
}

// elideRepeatedToolResponse returns a marker in place of payloads that repeat
// an earlier identical result, or nil to leave the response untouched.
func elideRepeatedToolResponse(in *hooks.Input) *hooks.Output {
	// Only tools the author declared read-only are eligible. A tool with side
	// effects may legitimately return identical output for two calls that each
	// did something (e.g. an append that was then undone), so eliding the
	// second would hide a real event.
	//
	// The category check is the second half of that: the hint is a declaration
	// rather than a proof, and for MCP tools the remote server supplies it.
	if !in.ToolReadOnly || !elidableCategories[in.ToolCategory] {
		return nil
	}
	// An error result is diagnostic: the model needs it every time, and a
	// repeated identical failure is itself information.
	if in.ToolError {
		return nil
	}
	// Without a session there is nothing to scope the state to.
	if in.SessionID == "" {
		return nil
	}

	payload, ok := in.ToolResponse.(string)
	if !ok {
		return nil
	}

	// Above this, limit_large_tool_results truncates first and wins, so an
	// elision marker would be built and then discarded.
	if len(payload) >= maxToolCallResultBytes {
		return nil
	}

	marker := elideMarker(in.ToolName, len(payload))
	// Eliding must actually shrink the conversation. Comparing against the real
	// marker keeps that true by construction, including for the long tool names
	// (MCP calls run to 30+ characters) where a fixed threshold does not.
	if len(payload) <= len(marker) {
		return nil
	}

	key, ok := elideCallKey(in.ToolName, in.ToolInput)
	if !ok {
		return nil
	}

	if !elideStore.observe(in.SessionID, key, sha256.Sum256([]byte(payload))) {
		return nil
	}

	return &hooks.Output{
		HookSpecificOutput: &hooks.HookSpecificOutput{
			HookEventName:       hooks.EventToolResponseTransform,
			UpdatedToolResponse: &marker,
		},
	}
}

// elideMarker is what replaces an elided payload. Worded to say explicitly that
// the tool ran, so the model does not treat it as a cache hit of unknown age.
func elideMarker(toolName string, payloadBytes int) string {
	return fmt.Sprintf(
		"[docker-agent] The %s tool ran and returned output byte-for-byte identical to its "+
			"earlier result for these same arguments in this session, so the %d-byte payload is "+
			"not repeated here. Nothing has changed since you last saw it.",
		toolName, payloadBytes)
}

// elideCallKey fingerprints a call as its tool name plus its arguments.
// [encoding/json] sorts map keys, so the result does not depend on Go's
// randomized map iteration order. Arguments that cannot be marshalled yield
// ok=false, which makes the caller leave the response untouched.
func elideCallKey(tool string, args map[string]any) (string, bool) {
	encoded, err := json.Marshal(args)
	if err != nil {
		return "", false
	}
	h := sha256.New()
	h.Write([]byte(tool))
	h.Write([]byte{0})
	h.Write(encoded)
	return string(h.Sum(nil)), true
}

// observe records sum as the latest output fingerprint for (session, key) and
// reports whether it repeats what was already recorded. A mismatch overwrites
// the stored fingerprint, so the *next* identical call elides.
func (s *elideState) observe(sessionID, key string, sum [sha256.Size]byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	perSession, ok := s.seen[sessionID]
	if !ok {
		// Drop the oldest tracked session when the cap is reached: cleanup
		// depends on a session_end entry the operator may not have wired, and a
		// session that ends abnormally never fires one.
		for len(s.order) >= maxElideSessions {
			delete(s.seen, s.order[0])
			s.order = s.order[1:]
		}
		perSession = make(map[string][sha256.Size]byte, 1)
		s.seen[sessionID] = perSession
		s.order = append(s.order, sessionID)
	}

	previous, seen := perSession[key]
	if seen {
		if previous == sum {
			return true
		}
		perSession[key] = sum
		return false
	}

	// New key: respect the per-session cap. Declining to record simply means
	// this call is never elided — correctness is unaffected.
	if len(perSession) < maxElideKeysPerSession {
		perSession[key] = sum
	}
	return false
}

// forget drops all state for a session.
func (s *elideState) forget(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[sessionID]; !ok {
		return
	}
	delete(s.seen, sessionID)
	s.order = slices.DeleteFunc(s.order, func(id string) bool { return id == sessionID })
}
