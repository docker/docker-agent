package toolsets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

// countingEmbedder records how often each input text is embedded.
type countingEmbedder struct {
	mu     sync.Mutex
	inputs map[string]int
}

func (c *countingEmbedder) count(text string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inputs[text]
}

func (c *countingEmbedder) record(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inputs[text]++
}

func (c *countingEmbedder) factory(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (provider.Provider, error) {
	return &countingEmbedderProvider{c}, nil
}

type countingEmbedderProvider struct{ c *countingEmbedder }

func (*countingEmbedderProvider) ID() modelsdev.ID        { return modelsdev.NewID("openai", "fake") }
func (*countingEmbedderProvider) BaseConfig() base.Config { return base.Config{} }

func (*countingEmbedderProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, errors.New("not implemented")
}

func (p *countingEmbedderProvider) CreateEmbedding(_ context.Context, text string) (*base.EmbeddingResult, error) {
	p.c.record(text)
	return &base.EmbeddingResult{Embedding: []float64{0.1, 0.2}, TotalTokens: 1}, nil
}

// Agents referencing one RAG definition must index and re-index each file
// once, not once per agent (https://github.com/docker/docker-agent/issues/4637).
func TestRAGReferencesShareOneIndex(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	docsDir := filepath.Join(dir, "knowledge")
	require.NoError(t, os.MkdirAll(docsDir, 0o755))
	note := filepath.Join(docsDir, "note.md")
	require.NoError(t, os.WriteFile(note, []byte("first version"), 0o644))

	yaml := fmt.Sprintf(`
rag:
  knowledge:
    docs: [%q]
    strategies:
      - type: chunked-embeddings
        embedding_model: openai/text-embedding-3-small
        vector_dimensions: 2
        database: %q
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    sub_agents: [a1, a2]
    toolsets: [{type: rag, ref: knowledge}]
  a1:
    model: openai/gpt-4o
    instruction: test
    toolsets: [{type: rag, ref: knowledge}]
  a2:
    model: openai/gpt-4o
    instruction: test
    toolsets: [{type: rag, ref: knowledge}]
`, docsDir, filepath.Join(dir, "rag.db"))

	embedder := &countingEmbedder{inputs: map[string]int{}}
	tm, err := teamloader.Load(t.Context(), config.NewBytesSource("agents.yaml", []byte(yaml)), &config.RuntimeConfig{
		EnvProviderOverride:    environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test"}),
		ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{}),
	},
		teamloader.WithProviderRegistry(provider.NewRegistry(map[string]provider.Factory{"openai": embedder.factory})),
		teamloader.WithToolsetRegistry(NewDefaultToolsetRegistry()),
	)
	require.NoError(t, err)
	defer func() { assert.NoError(t, tm.StopToolSets(t.Context())) }()

	for _, name := range tm.AgentNames() {
		a, err := tm.Agent(name)
		require.NoError(t, err)
		for _, ts := range a.ToolSets() {
			s, ok := tools.As[*tools.StartableToolSet](ts)
			require.True(t, ok)
			require.NoError(t, s.Start(t.Context()))
		}
	}
	assert.Equal(t, 1, embedder.count("first version"), "initial indexing runs once")

	// Retry writes after the watcher's 2s debounce, but observe completion promptly.
	var lastWrite time.Time
	require.Eventually(t, func() bool {
		if embedder.count("second version") > 0 {
			return true
		}
		if time.Since(lastWrite) >= 3*time.Second {
			_ = os.WriteFile(note, []byte("second version"), 0o644)
			lastWrite = time.Now()
		}
		return false
	}, 30*time.Second, 10*time.Millisecond)
	// Give any duplicate watcher time past its debounce to fire.
	assert.Never(t, func() bool { return embedder.count("second version") > 1 }, 3*time.Second, 100*time.Millisecond,
		"a change is re-embedded once")
}
