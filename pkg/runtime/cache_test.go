package runtime

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/cache"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func runWithCache(t *testing.T, c *cache.Cache, prov *messageRecordingProvider, sess *session.Session) []Event {
	t.Helper()

	root := agent.New("root", "You are a test agent",
		agent.WithModel(prov),
		agent.WithCache(c),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	sess.Title = "cache test"

	evCh := rt.RunStream(t.Context(), sess)
	var events []Event
	for ev := range evCh {
		events = append(events, ev)
	}
	return events
}

// TestCache_StoresAndReplaysAnswer verifies that a cache miss invokes the model
// once, and a subsequent identical question is served from the cache without
// hitting the model again.
func TestCache_StoresAndReplaysAnswer(t *testing.T) {
	c, err := cache.New(cache.Config{Enabled: true})
	require.NoError(t, err)
	require.NotNil(t, c)

	stream := newStreamBuilder().
		AddContent("The answer is 42").
		AddStopWithUsage(5, 3).
		Build()
	prov := &messageRecordingProvider{
		id:      "test/mock-model",
		streams: []*mockStream{stream},
	}

	// First turn: cache miss → model is called once and answer stored.
	sess1 := session.New(session.WithUserMessage("What is the answer?"))
	events1 := runWithCache(t, c, prov, sess1)

	require.NotEmpty(t, events1)
	assert.IsType(t, &StreamStoppedEvent{}, events1[len(events1)-1])

	prov.mu.Lock()
	require.Len(t, prov.recordedMessages, 1, "expected exactly one model call on first turn (cache miss)")
	prov.mu.Unlock()

	// Verify the answer was emitted.
	require.True(t, hasAgentChoice(events1, "The answer is 42"), "expected first run to emit the model answer")

	// Second turn with the same question: cache hit → no extra model call.
	sess2 := session.New(session.WithUserMessage("What is the answer?"))
	runWithCache(t, c, prov, sess2)

	prov.mu.Lock()
	defer prov.mu.Unlock()
	assert.Len(t, prov.recordedMessages, 1, "second turn must NOT call the model again")

	// The cached response must have been added as the assistant message.
	require.True(t, hasAssistantMessage(sess2, "The answer is 42"), "expected cached response in session")
}

// TestCache_CaseInsensitive verifies that case-insensitive matching is the
// default when CaseSensitive is false.
func TestCache_CaseInsensitive(t *testing.T) {
	c, err := cache.New(cache.Config{Enabled: true, CaseSensitive: false})
	require.NoError(t, err)

	stream := newStreamBuilder().
		AddContent("Hi there!").
		AddStopWithUsage(5, 3).
		Build()
	prov := &messageRecordingProvider{
		id:      "test/mock-model",
		streams: []*mockStream{stream},
	}

	// First turn: "Hello" → model is called.
	sess1 := session.New(session.WithUserMessage("Hello"))
	runWithCache(t, c, prov, sess1)

	prov.mu.Lock()
	require.Len(t, prov.recordedMessages, 1)
	prov.mu.Unlock()

	// Second turn: "HELLO" must still hit the cache.
	sess2 := session.New(session.WithUserMessage("HELLO"))
	runWithCache(t, c, prov, sess2)

	prov.mu.Lock()
	defer prov.mu.Unlock()
	assert.Len(t, prov.recordedMessages, 1, "case-insensitive cache must hit on different case")
	assert.True(t, hasAssistantMessage(sess2, "Hi there!"), "expected cached response to be replayed")
}

// TestCache_TrimSpaces verifies that whitespace trimming is applied when
// TrimSpaces is enabled.
func TestCache_TrimSpaces(t *testing.T) {
	c, err := cache.New(cache.Config{Enabled: true, TrimSpaces: true})
	require.NoError(t, err)

	stream := newStreamBuilder().
		AddContent("Trimmed!").
		AddStopWithUsage(5, 3).
		Build()
	prov := &messageRecordingProvider{
		id:      "test/mock-model",
		streams: []*mockStream{stream},
	}

	// First turn: question with surrounding whitespace.
	sess1 := session.New(session.WithUserMessage("  Hello  "))
	runWithCache(t, c, prov, sess1)

	prov.mu.Lock()
	require.Len(t, prov.recordedMessages, 1)
	prov.mu.Unlock()

	// Second turn: same question without whitespace must hit the cache.
	sess2 := session.New(session.WithUserMessage("Hello"))
	runWithCache(t, c, prov, sess2)

	prov.mu.Lock()
	defer prov.mu.Unlock()
	assert.Len(t, prov.recordedMessages, 1, "trim-enabled cache must hit when whitespace differs")
	assert.True(t, hasAssistantMessage(sess2, "Trimmed!"))
}

// TestCache_DisabledHasNoEffect verifies that when the agent has no cache
// attached, every call goes through to the model.
// TestCache_StorageIsAStopHookBuiltin asserts the architectural contract
// that the cache_response storage is wired through the new hook system as
// a stop-hook builtin (not as a hard-coded runtime call). It is the
// regression test for the migration to the hooks mechanism.
func TestCache_StorageIsAStopHookBuiltin(t *testing.T) {
	c, err := cache.New(cache.Config{Enabled: true})
	require.NoError(t, err)

	root := agent.New("root", "You are a test agent",
		agent.WithModel(&messageRecordingProvider{id: "test/mock"}),
		agent.WithCache(c),
	)
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// The runtime should have auto-injected a stop hook of type=builtin
	// pointing at BuiltinCacheResponse for an agent that has a cache.
	exec := rt.hooksExec(root)
	require.NotNil(t, exec, "a cache-enabled agent must have a hooks executor")
	require.True(t, exec.Has(hooks.EventStop), "cache-enabled agent must have a stop hook")

	// The cache_response builtin must be registered on the runtime's
	// private hooks registry; that's the seam that lets the closure
	// reach back to a.Cache() through Input.AgentName.
	fn, ok := rt.hooksRegistry.LookupBuiltin(BuiltinCacheResponse)
	require.True(t, ok, "cache_response builtin must be registered")
	require.NotNil(t, fn)
}

func TestCache_DisabledHasNoEffect(t *testing.T) {
	stream1 := newStreamBuilder().AddContent("first").AddStopWithUsage(5, 3).Build()
	stream2 := newStreamBuilder().AddContent("second").AddStopWithUsage(5, 3).Build()

	prov := &messageRecordingProvider{
		id:      "test/mock-model",
		streams: []*mockStream{stream1, stream2},
	}

	sess1 := session.New(session.WithUserMessage("Same question"))
	runWithCache(t, nil, prov, sess1)
	sess2 := session.New(session.WithUserMessage("Same question"))
	runWithCache(t, nil, prov, sess2)

	prov.mu.Lock()
	defer prov.mu.Unlock()
	assert.Len(t, prov.recordedMessages, 2, "without a cache, every turn must hit the model")
}

// TestCache_EmptyResponseIsNotCached documents that the cache_response
// stop-hook deliberately drops empty (whitespace-only) assistant
// responses on the floor: replaying nothing on a future turn would leave
// the user staring at a blank reply with no recourse but to ask again,
// so the model is called every time.
func TestCache_EmptyResponseIsNotCached(t *testing.T) {
	c, err := cache.New(cache.Config{Enabled: true})
	require.NoError(t, err)

	// Two empty-response streams; we expect the model to be called both
	// times because the empty answer must never reach the cache.
	stream1 := newStreamBuilder().AddContent("").AddStopWithUsage(5, 0).Build()
	stream2 := newStreamBuilder().AddContent("").AddStopWithUsage(5, 0).Build()
	prov := &messageRecordingProvider{
		id:      "test/mock-model",
		streams: []*mockStream{stream1, stream2},
	}

	sess1 := session.New(session.WithUserMessage("silent treatment"))
	runWithCache(t, c, prov, sess1)
	sess2 := session.New(session.WithUserMessage("silent treatment"))
	runWithCache(t, c, prov, sess2)

	prov.mu.Lock()
	defer prov.mu.Unlock()
	assert.Len(t, prov.recordedMessages, 2,
		"empty responses must not be cached; the model must be called every time")

	_, found := c.Lookup("silent treatment")
	assert.False(t, found, "empty assistant response must not appear in the cache")
}

func hasAgentChoice(events []Event, content string) bool {
	for _, ev := range events {
		if ac, ok := ev.(*AgentChoiceEvent); ok && strings.Contains(ac.Content, content) {
			return true
		}
	}
	return false
}

func hasAssistantMessage(sess *session.Session, content string) bool {
	for _, item := range sess.Messages {
		if item.IsMessage() && item.Message.Message.Role == chat.MessageRoleAssistant &&
			strings.Contains(item.Message.Message.Content, content) {
			return true
		}
	}
	return false
}
