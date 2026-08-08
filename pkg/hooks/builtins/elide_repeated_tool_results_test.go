package builtins

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
)

// bigPayload returns a payload comfortably above the marker length and below
// the limit_large_tool_results threshold.
func bigPayload(seed string) string {
	return seed + strings.Repeat("x", 1024)
}

// forgetAllElideState resets the package-level store between tests. These tests
// deliberately do not run in parallel with each other: they share that store,
// which is the same state the runtime shares across a process.
func forgetAllElideState() {
	elideStore.mu.Lock()
	defer elideStore.mu.Unlock()
	elideStore.seen = make(map[string]map[string][32]byte)
	elideStore.order = nil
}

// elideStoreSessions reports how many sessions are currently tracked.
func elideStoreSessions() int {
	elideStore.mu.Lock()
	defer elideStore.mu.Unlock()
	return len(elideStore.seen)
}

// elideStoreLen reports how many call keys are recorded for a session.
func elideStoreLen(sessionID string) int {
	elideStore.mu.Lock()
	defer elideStore.mu.Unlock()
	return len(elideStore.seen[sessionID])
}

func transformInput(sessionID, tool, payload string, args map[string]any) *hooks.Input {
	return &hooks.Input{
		HookEventName: hooks.EventToolResponseTransform,
		SessionID:     sessionID,
		ToolName:      tool,
		ToolCategory:  "filesystem",
		ToolReadOnly:  true,
		ToolInput:     args,
		ToolResponse:  payload,
	}
}

func elide(t *testing.T, in *hooks.Input) *hooks.Output {
	t.Helper()
	out, err := elideRepeatedToolResults(t.Context(), in, nil)
	require.NoError(t, err)
	return out
}

func TestElideRepeatedToolResults_FirstCallPassesThrough(t *testing.T) {
	forgetAllElideState()
	payload := bigPayload("contents")

	out := elide(t, transformInput("s1", "read_file", payload, map[string]any{"path": "a.txt"}))
	assert.Nil(t, out, "the first result must reach the model in full")
}

func TestElideRepeatedToolResults_IdenticalRepeatIsElided(t *testing.T) {
	forgetAllElideState()
	payload := bigPayload("contents")
	args := map[string]any{"path": "a.txt"}

	require.Nil(t, elide(t, transformInput("s1", "read_file", payload, args)))

	out := elide(t, transformInput("s1", "read_file", payload, args))
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse)

	got := *out.HookSpecificOutput.UpdatedToolResponse
	assert.NotEqual(t, payload, got)
	assert.Less(t, len(got), len(payload), "the marker must be smaller than the payload it replaces")
	assert.Contains(t, got, "read_file")
	assert.Contains(t, got, "identical")
}

// THE consistency property: the payload is only ever elided when the tool's
// fresh output is byte-for-byte identical to what the model already saw. The
// tool always executes, so a changed file can never be served from cache.
func TestElideRepeatedToolResults_ChangedOutputIsNeverElided(t *testing.T) {
	forgetAllElideState()
	args := map[string]any{"path": "a.txt"}
	first := bigPayload("version-one")
	second := bigPayload("version-two")

	require.Nil(t, elide(t, transformInput("s1", "read_file", first, args)))

	out := elide(t, transformInput("s1", "read_file", second, args))
	assert.Nil(t, out, "changed output must always reach the model in full")

	// And the new output becomes the baseline, so a repeat of *it* elides
	// while a return to the old content does not.
	require.NotNil(t, elide(t, transformInput("s1", "read_file", second, args)))
	assert.Nil(t, elide(t, transformInput("s1", "read_file", first, args)),
		"reverting to earlier content must reach the model in full")
}

func TestElideRepeatedToolResults_NonReadOnlyToolIsNeverElided(t *testing.T) {
	forgetAllElideState()
	payload := bigPayload("side effects")
	args := map[string]any{"cmd": "date"}

	in := transformInput("s1", "shell", payload, args)
	in.ToolReadOnly = false
	require.Nil(t, elide(t, in))

	in2 := transformInput("s1", "shell", payload, args)
	in2.ToolReadOnly = false
	assert.Nil(t, elide(t, in2), "a tool with side effects must never be elided")
}

func TestElideRepeatedToolResults_ErrorResultIsNeverElided(t *testing.T) {
	forgetAllElideState()
	payload := bigPayload("boom")
	args := map[string]any{"path": "a.txt"}

	in := transformInput("s1", "read_file", payload, args)
	in.ToolError = true
	require.Nil(t, elide(t, in))

	in2 := transformInput("s1", "read_file", payload, args)
	in2.ToolError = true
	assert.Nil(t, elide(t, in2))
}

func TestElideRepeatedToolResults_DifferentArgsAreDistinct(t *testing.T) {
	forgetAllElideState()
	payload := bigPayload("same bytes")

	require.Nil(t, elide(t, transformInput("s1", "read_file", payload, map[string]any{"path": "a.txt"})))
	assert.Nil(t, elide(t, transformInput("s1", "read_file", payload, map[string]any{"path": "b.txt"})),
		"a different argument set is a different call")
}

// Key building must not depend on Go's randomized map iteration order.
func TestElideRepeatedToolResults_ArgOrderIsIrrelevant(t *testing.T) {
	forgetAllElideState()
	payload := bigPayload("stable")

	require.Nil(t, elide(t, transformInput("s1", "read_file", payload,
		map[string]any{"path": "a.txt", "line": 1, "limit": 20})))

	for range 20 {
		out := elide(t, transformInput("s1", "read_file", payload,
			map[string]any{"limit": 20, "line": 1, "path": "a.txt"}))
		require.NotNil(t, out, "identical args in any map order must be the same key")
	}
}

func TestElideRepeatedToolResults_SessionsAreIsolated(t *testing.T) {
	forgetAllElideState()
	payload := bigPayload("contents")
	args := map[string]any{"path": "a.txt"}

	require.Nil(t, elide(t, transformInput("s1", "read_file", payload, args)))
	assert.Nil(t, elide(t, transformInput("s2", "read_file", payload, args)),
		"another session has not seen this output")
}

func TestElideRepeatedToolResults_SmallPayloadNotWorthEliding(t *testing.T) {
	forgetAllElideState()
	small := "tiny"
	args := map[string]any{"path": "a.txt"}

	require.Nil(t, elide(t, transformInput("s1", "read_file", small, args)))
	assert.Nil(t, elide(t, transformInput("s1", "read_file", small, args)),
		"eliding a payload smaller than the marker would cost tokens, not save them")
}

func TestElideRepeatedToolResults_SessionEndForgetsState(t *testing.T) {
	forgetAllElideState()
	payload := bigPayload("contents")
	args := map[string]any{"path": "a.txt"}

	require.Nil(t, elide(t, transformInput("s1", "read_file", payload, args)))
	require.NotNil(t, elide(t, transformInput("s1", "read_file", payload, args)))

	_, err := elideRepeatedToolResults(t.Context(), &hooks.Input{
		HookEventName: hooks.EventSessionEnd,
		SessionID:     "s1",
	}, nil)
	require.NoError(t, err)

	assert.Nil(t, elide(t, transformInput("s1", "read_file", payload, args)),
		"state must be dropped when the session ends")
}

func TestElideRepeatedToolResults_PerSessionKeyCapIsBounded(t *testing.T) {
	forgetAllElideState()
	payload := bigPayload("contents")

	// Fill past the cap with distinct argument sets.
	for i := range maxElideKeysPerSession + 50 {
		elide(t, transformInput("s1", "read_file", payload, map[string]any{"path": i}))
	}
	assert.LessOrEqual(t, elideStoreLen("s1"), maxElideKeysPerSession,
		"per-session key count must stay bounded")
}

// Parallel tool calls dispatch this hook concurrently; run under -race.
func TestElideRepeatedToolResults_ConcurrentDispatch(t *testing.T) {
	forgetAllElideState()
	payload := bigPayload("contents")

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			for range 8 {
				_, err := elideRepeatedToolResults(t.Context(),
					transformInput("s1", "read_file", payload, map[string]any{"path": i % 4}), nil)
				assert.NoError(t, err)
			}
		})
	}
	wg.Wait()
}

func TestElideRepeatedToolResults_IsRegistered(t *testing.T) {
	forgetAllElideState()
	reg := hooks.NewRegistry()
	require.NoError(t, Register(reg))

	handler, ok := reg.LookupBuiltin(ElideRepeatedToolResults)
	require.Truef(t, ok, "builtin %q must be registered", ElideRepeatedToolResults)

	payload := bigPayload("contents")
	args := map[string]any{"path": "a.txt"}

	first, err := handler(t.Context(), transformInput("s9", "read_file", payload, args), nil)
	require.NoError(t, err)
	require.Nil(t, first)

	second, err := handler(t.Context(), transformInput("s9", "read_file", payload, args), nil)
	require.NoError(t, err)
	require.NotNil(t, second)
}

func TestElideRepeatedToolResults_UnsupportedEventIsNoOp(t *testing.T) {
	forgetAllElideState()
	out, err := elideRepeatedToolResults(t.Context(), &hooks.Input{
		HookEventName: hooks.EventTurnStart,
		SessionID:     "s1",
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestElideRepeatedToolResults_NilInput(t *testing.T) {
	forgetAllElideState()
	out, err := elideRepeatedToolResults(t.Context(), nil, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

// limit_large_tool_results is auto-injected at the FRONT of
// tool_response_transform and the first non-nil rewrite in config order wins, so
// for payloads it handles an elision marker would be built and then thrown away.
// Declining to act keeps the state honest instead of recording a fingerprint for
// a marker that never reaches the model.
func TestElideRepeatedToolResults_DeclinesPayloadsLimitLargeWillTruncate(t *testing.T) {
	forgetAllElideState()

	huge := strings.Repeat("x", maxToolCallResultBytes+1)
	args := map[string]any{"path": "big.txt"}

	require.Nil(t, elide(t, transformInput("s1", "read_file", huge, args)))
	assert.Nil(t, elide(t, transformInput("s1", "read_file", huge, args)),
		"a payload limit_large_tool_results will truncate must not be elided")
	assert.Zero(t, elideStoreLen("s1"),
		"and must not consume state for a marker that would be discarded")
}

// Compaction drops the messages a fingerprint stands for. Keeping it would tell
// the model "nothing has changed" about bytes it can no longer see — for the
// rest of the session, since only a byte change would release the payload again.
func TestElideRepeatedToolResults_CompactionForgetsState(t *testing.T) {
	forgetAllElideState()
	payload := bigPayload("contents")
	args := map[string]any{"path": "a.txt"}

	require.Nil(t, elide(t, transformInput("s1", "read_file", payload, args)))
	require.NotNil(t, elide(t, transformInput("s1", "read_file", payload, args)))

	_, err := elideRepeatedToolResults(t.Context(), &hooks.Input{
		HookEventName: hooks.EventAfterCompaction,
		SessionID:     "s1",
	}, nil)
	require.NoError(t, err)

	assert.Nil(t, elide(t, transformInput("s1", "read_file", payload, args)),
		"after compaction the payload must be re-sent, not marked unchanged")
}

func TestElideRepeatedToolResults_SessionStartForgetsState(t *testing.T) {
	forgetAllElideState()
	payload := bigPayload("contents")
	args := map[string]any{"path": "a.txt"}

	require.Nil(t, elide(t, transformInput("s1", "read_file", payload, args)))
	require.NotNil(t, elide(t, transformInput("s1", "read_file", payload, args)))

	for _, source := range []string{"compact", "clear", "resume"} {
		_, err := elideRepeatedToolResults(t.Context(), &hooks.Input{
			HookEventName: hooks.EventSessionStart,
			SessionID:     "s1",
			Source:        source,
		}, nil)
		require.NoError(t, err)

		assert.Nilf(t, elide(t, transformInput("s1", "read_file", payload, args)),
			"session_start %q rebuilds the context, so state must be dropped", source)
		require.NotNil(t, elide(t, transformInput("s1", "read_file", payload, args)))
	}
}

// ReadOnlyHint is a declaration, not a proof: built-in tools set it for
// approval-gating reasons while still having effects, and for MCP tools the
// remote server supplies it.
func TestElideRepeatedToolResults_CategoryMustAlsoBeElidable(t *testing.T) {
	forgetAllElideState()
	payload := bigPayload("contents")
	args := map[string]any{"q": "x"}

	// A read-only-declaring tool from a category this builtin does not own.
	mk := func() *hooks.Input {
		in := transformInput("s1", "search", payload, args)
		in.ToolCategory = "mcp"
		return in
	}
	require.Nil(t, elide(t, mk()))
	assert.Nil(t, elide(t, mk()),
		"a self-declared read-only MCP tool must not be able to suppress its own output")

	// An empty category (tool unknown to the agent) is equally inert.
	forgetAllElideState()
	mkEmpty := func() *hooks.Input {
		in := transformInput("s1", "read_file", payload, args)
		in.ToolCategory = ""
		return in
	}
	require.Nil(t, elide(t, mkEmpty()))
	assert.Nil(t, elide(t, mkEmpty()))
}

// A fixed byte threshold does not hold for long tool names: the marker embeds
// the name, so an MCP call at 30+ characters produces a marker longer than the
// payload it would replace.
func TestElideRepeatedToolResults_NeverGrowsTheConversation(t *testing.T) {
	forgetAllElideState()

	const longName = "mcp__github__list_pull_requests"
	// Above the old fixed 256-byte threshold would have been elided; the marker
	// for a name this long is larger still.
	payload := strings.Repeat("y", 250)
	args := map[string]any{"path": "a"}

	require.Greater(t, len(elideMarker(longName, len(payload))), len(payload),
		"fixture must exercise the case where the marker is the larger of the two")

	in := func() *hooks.Input {
		i := transformInput("s1", longName, payload, args)
		i.ToolCategory = "filesystem"
		return i
	}
	require.Nil(t, elide(t, in()))
	assert.Nil(t, elide(t, in()),
		"eliding must never replace a payload with something longer")
}

// Cleanup depends on a session_end entry the operator has to wire up, and an
// abnormally-ended session never fires one.
func TestElideRepeatedToolResults_SessionCountIsBounded(t *testing.T) {
	forgetAllElideState()
	payload := bigPayload("contents")

	for i := range maxElideSessions + 100 {
		elide(t, transformInput(fmt.Sprintf("s%d", i), "read_file", payload, map[string]any{"path": "a"}))
	}
	assert.LessOrEqual(t, elideStoreSessions(), maxElideSessions,
		"tracked session count must stay bounded without session_end")
}
