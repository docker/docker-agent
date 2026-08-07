package builtins

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
)

// bigPayload returns a payload comfortably above minElidableBytes.
func bigPayload(marker string) string {
	return marker + strings.Repeat("x", minElidableBytes*2)
}

// forgetAllElideState resets the package-level store between tests. These tests
// deliberately do not run in parallel with each other: they share that store,
// which is the same state the runtime shares across a process.
func forgetAllElideState() {
	elideStore.mu.Lock()
	defer elideStore.mu.Unlock()
	elideStore.seen = make(map[string]map[string][32]byte)
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
