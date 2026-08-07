package builtins

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
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
// The saving is in tokens, not in I/O — a repeated 40 KiB read_file result
// becomes a one-line marker. Latency is unchanged because the tool still runs.
const ElideRepeatedToolResults = "elide_repeated_tool_results"

const (
	// minElidableBytes is the payload size below which eliding is a net loss:
	// the marker itself costs tokens, so replacing a short result with it would
	// make the conversation bigger, not smaller.
	minElidableBytes = 256

	// maxElideKeysPerSession bounds per-session memory. A session that calls
	// read-only tools with thousands of distinct argument sets stops recording
	// new fingerprints rather than growing without limit; already-recorded keys
	// keep working. Each entry is a 32-byte hash plus a map key.
	maxElideKeysPerSession = 4096
)

// elideState remembers, per session, the fingerprint of the most recent output
// seen for each (tool, arguments) pair.
//
// Package-level state mirrors the limit_large_tool_results builtin, which keeps
// per-session scratch state for the same reason: builtins are registered as
// plain functions and have nowhere else to live. Entries are dropped on
// session_end.
type elideState struct {
	mu sync.Mutex
	// seen maps session ID -> call key -> sha256 of the last output.
	seen map[string]map[string][sha256.Size]byte
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
	if !in.ToolReadOnly {
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
	if !ok || len(payload) < minElidableBytes {
		return nil
	}

	key, ok := elideCallKey(in.ToolName, in.ToolInput)
	if !ok {
		return nil
	}

	if !elideStore.observe(in.SessionID, key, sha256.Sum256([]byte(payload))) {
		return nil
	}

	marker := fmt.Sprintf(
		"[docker-agent] The %s tool ran and returned output byte-for-byte identical to its "+
			"earlier result for these same arguments in this session, so the %d-byte payload is "+
			"not repeated here. Nothing has changed since you last saw it.",
		in.ToolName, len(payload))

	return &hooks.Output{
		HookSpecificOutput: &hooks.HookSpecificOutput{
			HookEventName:       hooks.EventToolResponseTransform,
			UpdatedToolResponse: &marker,
		},
	}
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
		perSession = make(map[string][sha256.Size]byte, 1)
		s.seen[sessionID] = perSession
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
	delete(s.seen, sessionID)
}
