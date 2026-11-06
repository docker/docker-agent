package runtime

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic/anthropictest"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/providers"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
)

// claude5Runtime runs a real Claude Sonnet 5.5 client, built by the provider
// registry, against server, with a 40k-token context window.
func claude5Runtime(t *testing.T, server *anthropictest.Server, providerOpts map[string]any, store session.Store) *LocalRuntime {
	t.Helper()
	return anthropicRuntime(t, server, "claude-sonnet-5-5", providerOpts, store,
		mockModelStoreWithCostAndLimit{limit: 40_000, cost: modelsdev.Cost{Input: 1, Output: 2}})
}

// anthropicRuntime runs a real Anthropic client for model, built by the
// provider registry, against server, priced and sized by models.
func anthropicRuntime(t *testing.T, server *anthropictest.Server, modelName string, providerOpts map[string]any, store session.Store, models ModelStore, agentOpts ...agent.Opt) *LocalRuntime {
	t.Helper()
	target, err := url.Parse(server.URL())
	require.NoError(t, err)
	reroute := options.WithHTTPTransportWrapper(func(next http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			req = req.Clone(req.Context())
			req.URL.Scheme, req.URL.Host, req.Host = target.Scheme, target.Host, target.Host
			return next.RoundTrip(req)
		})
	})
	cfg := latest.ModelConfig{Provider: "anthropic", Model: modelName, ProviderOpts: providerOpts}
	env := environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "test-key"})
	model, err := providers.NewDefaultRegistry().New(t.Context(), &cfg, env, reroute)
	require.NoError(t, err)

	root := agent.New("root", "You are terse.", append([]agent.Opt{agent.WithModel(model)}, agentOpts...)...)
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionStore(store),
		WithModelStore(models))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, rt.Close()) })
	return rt
}

func openSessionStore(t *testing.T, path string) session.Store {
	t.Helper()
	store, err := sqlitestore.New(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close()) })
	return store
}

func requireNoErrorEvents(t *testing.T, events []Event) {
	t.Helper()
	for _, ev := range events {
		if e, ok := ev.(*ErrorEvent); ok {
			t.Fatalf("unexpected error event: %s", e.Error)
		}
	}
}

// A thinking turn that fills the context is summarized by threshold
// compaction; after a reload, the next request carries the summary and the
// kept turn's signed thinking verbatim, under the drop_block binding.
func TestClaude5LocalCompactionLifecycle(t *testing.T) {
	t.Parallel()

	server := anthropictest.NewServer(t,
		anthropictest.ThinkingTextReply("msg_1", "plan", "sig-1", "a1", 36_500),
		anthropictest.TextReply("msg_summary", "SUMMARY", 100),
		anthropictest.TextReply("msg_2", "a2", 100),
		anthropictest.TextReply("msg_3", "a3", 100),
	)
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store := openSessionStore(t, dbPath)
	rt := claude5Runtime(t, server, nil, store)

	// 36.5k reported tokens cross 90% of the 40k window. Each ~16k-token
	// question fits the summary request (a quarter of the window is reserved
	// for the summary) but not the kept tail (a fifth), so the thinking turn
	// and u2 are kept. The seed goes through the store, as earlier turns would.
	background := strings.Repeat("background ", 6_000)
	sess := session.New(session.WithMessages([]session.Item{
		session.NewMessageItem(session.UserMessage("u0 " + background)),
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "noted"}}),
	}))
	require.NoError(t, store.AddSession(t.Context(), sess))
	sess.AddMessage(session.UserMessage("u1 " + background))
	requireNoErrorEvents(t, drainRunStream(t, rt, sess))
	sess.AddMessage(session.UserMessage("u2"))
	events := drainRunStream(t, rt, sess)
	requireNoErrorEvents(t, events)
	require.Equal(t, 1, countCompactionStarts(events), "the 36.5k-token turn crosses 90% of the 40k window")

	loaded, err := openSessionStore(t, dbPath).GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	loaded.AddMessage(session.UserMessage("u3"))
	reloaded := claude5Runtime(t, server, nil, openSessionStore(t, dbPath))
	requireNoErrorEvents(t, drainRunStream(t, reloaded, loaded))

	requests := server.Requests()
	require.Len(t, requests, 4, "turn 1, summary, turn 2, turn 3 after reload")
	last := requests[3]
	assert.Contains(t, last.Raw, "SUMMARY", "the summary replaces the compacted history")
	assert.NotContains(t, last.Raw, "background", "the long question was summarized")
	assert.Contains(t, last.Raw, `"text":"u2"`, "the kept tail keeps the latest question")
	assert.Contains(t, last.Raw, `"signature":"sig-1"`, "the kept turn's signed thinking is replayed verbatim")
	thinking, _ := last.Body["thinking"].(map[string]any)
	binding, _ := thinking["block_binding"].(map[string]any)
	assert.Equal(t, "drop_block", binding["prefix_mismatch_behavior"])
}

// Native compaction stores the signed block and its per-iteration usage;
// after a reload, the next request replays the block verbatim with the
// compaction beta, and the summary's cost covers every iteration.
func TestClaude5NativeCompactionLifecycle(t *testing.T) {
	t.Parallel()

	server := anthropictest.NewServer(t,
		anthropictest.TextReply("msg_1", "a1", 36_500),
		anthropictest.CompactionReply("msg_compact", "SUMMARY", "sig-c", [2]int64{30_000, 400}, [2]int64{3_000, 600}),
		anthropictest.TextReply("msg_2", "a2", 100),
		anthropictest.TextReply("msg_3", "a3", 100),
	)
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	native := map[string]any{"native_compaction": true}
	rt := claude5Runtime(t, server, native, openSessionStore(t, dbPath))

	sess := session.New(session.WithUserMessage("u1"))
	requireNoErrorEvents(t, drainRunStream(t, rt, sess))
	sess.AddMessage(session.UserMessage("u2"))
	events := drainRunStream(t, rt, sess)
	requireNoErrorEvents(t, events)
	require.Equal(t, 1, countCompactionStarts(events), "the 36.5k-token turn crosses 90% of the 40k window")

	loaded, err := openSessionStore(t, dbPath).GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	var summary *session.Item
	for i := range loaded.Messages {
		if loaded.Messages[i].Summary != "" {
			summary = &loaded.Messages[i]
		}
	}
	require.NotNil(t, summary)
	require.NotNil(t, summary.Compaction)
	assert.Contains(t, string(summary.Compaction.Block), `"signature":"sig-c"`)
	assert.Len(t, summary.Compaction.Iterations, 2)
	// $1/$2 per MTok: (30,000 + 2×400 + 3,000 + 2×600) / 1M.
	assert.InDelta(t, 0.035, summary.Cost, 1e-9)

	loaded.AddMessage(session.UserMessage("u3"))
	reloaded := claude5Runtime(t, server, native, openSessionStore(t, dbPath))
	requireNoErrorEvents(t, drainRunStream(t, reloaded, loaded))

	requests := server.Requests()
	require.Len(t, requests, 4, "turn 1, compaction, turn 2, turn 3 after reload")
	last := requests[3]
	assert.Contains(t, last.Betas, "compact-2026-09-04")
	messages, _ := last.Body["messages"].([]any)
	require.NotEmpty(t, messages)
	first, _ := json.Marshal(messages[0])
	assert.Contains(t, string(first), `"type":"compaction"`, "the conversation resumes from the compaction block")
	assert.Contains(t, string(first), `"signature":"sig-c"`, "the block is replayed verbatim")
	assert.Contains(t, string(first), `"encrypted_content":"enc-msg_compact"`)
	assert.Len(t, messages, 3, "block, a2, u3: the compacted history is not resent")
}
