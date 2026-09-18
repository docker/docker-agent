package rag

import (
	"context"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goccy/go-yaml"
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
	"github.com/docker/docker-agent/pkg/rag/database"
	"github.com/docker/docker-agent/pkg/rag/strategy"
	"github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/tools"
)

// keywordModel is a tiny offline model: embeddings count the words "cat" and
// "dog", chat completions echo the prompt (so semantic summaries keep the
// keywords) and reranking scores documents by how often the query's first
// word appears. Embedding calls are counted to prove nothing is re-indexed.
type keywordModel struct {
	embeddings atomic.Int64
}

func (m *keywordModel) ID() modelsdev.ID        { return modelsdev.NewID("mock", "keyword") }
func (m *keywordModel) BaseConfig() base.Config { return base.Config{} }

func (m *keywordModel) CreateEmbedding(_ context.Context, text string) (*base.EmbeddingResult, error) {
	m.embeddings.Add(1)
	lower := strings.ToLower(text)
	return &base.EmbeddingResult{
		Embedding:   []float64{float64(strings.Count(lower, "cat")), float64(strings.Count(lower, "dog")), 0.1},
		TotalTokens: 1,
	}, nil
}

func (m *keywordModel) CreateChatCompletionStream(_ context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	return &echoStream{content: messages[len(messages)-1].Content}, nil
}

func (m *keywordModel) Rerank(_ context.Context, query string, documents []types.Document, _ string) ([]float64, error) {
	keyword := strings.ToLower(strings.Fields(query)[0])
	scores := make([]float64, len(documents))
	for i, doc := range documents {
		scores[i] = float64(strings.Count(strings.ToLower(doc.Content), keyword))
	}
	return scores, nil
}

type echoStream struct {
	content string
	sent    bool
}

func (s *echoStream) Recv() (chat.MessageStreamResponse, error) {
	if s.sent {
		return chat.MessageStreamResponse{}, io.EOF
	}
	s.sent = true
	return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: s.content}}}}, nil
}

func (s *echoStream) Close() {}

var testDocuments = Documents{
	"pets/cats.md":  []byte("Cats purr. A cat sleeps most of the day and a cat hunts at night."),
	"pets/dogs.md":  []byte("Dogs bark. A dog fetches sticks and a dog guards the house."),
	"notes/todo.md": []byte("Buy milk, call the plumber, water the plants."),
}

// newDocumentsManager builds a manager for ragYAML over testDocuments, with
// every strategy database pointed at a temp dir so native runs leave the data
// dir alone.
func newDocumentsManager(t *testing.T, ragYAML string, model *keywordModel, docs Documents) (*Manager, error) {
	t.Helper()
	var ragCfg latest.RAGConfig
	require.NoError(t, yaml.Unmarshal([]byte(strings.ReplaceAll(ragYAML, "$TMP", filepath.ToSlash(t.TempDir()))), &ragCfg))

	registry := provider.NewRegistry(map[string]provider.Factory{
		"mock": func(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (provider.Provider, error) {
			return model, nil
		},
	})
	runConfig := &config.RuntimeConfig{
		EnvProviderOverride:    environment.NewMapEnvProvider(nil),
		ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{}),
		ProviderRegistry:       registry,
	}
	mgr, err := NewManager(t.Context(), "pets", &ragCfg, ManagersBuildConfig{
		ParentDir:     "/",
		Env:           runConfig.EnvProvider(),
		RuntimeConfig: runConfig,
		Documents:     docs,
	})
	if err == nil {
		t.Cleanup(func() { require.NoError(t, mgr.Close()) })
	}
	return mgr, err
}

func sourcePaths(results []database.SearchResult) []string {
	var paths []string
	for _, r := range results {
		paths = append(paths, r.Document.SourcePath)
	}
	return paths
}

func TestDocumentsAreIndexedByEveryStrategy(t *testing.T) {
	t.Parallel()
	for name, ragYAML := range map[string]string{
		"bm25": `
docs: [pets]
strategies:
  - type: bm25
    database: $TMP/bm25.db
`,
		"chunked-embeddings": `
docs: [pets]
strategies:
  - type: chunked-embeddings
    database: $TMP/chunked.db
    embedding_model: mock/keyword
    vector_dimensions: 3
    threshold: 0
`,
		"semantic-embeddings with reranking": `
docs: [pets]
strategies:
  - type: semantic-embeddings
    database: $TMP/semantic.db
    embedding_model: mock/keyword
    chat_model: mock/keyword
    vector_dimensions: 3
    threshold: 0
results:
  reranking:
    model: mock/keyword
`,
		"fusion of bm25 and chunked-embeddings": `
docs: [pets]
strategies:
  - type: bm25
    database: $TMP/bm25.db
  - type: chunked-embeddings
    database: $TMP/chunked.db
    embedding_model: mock/keyword
    vector_dimensions: 3
    threshold: 0
results:
  limit: 2
`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var model keywordModel
			mgr, err := newDocumentsManager(t, ragYAML, &model, testDocuments)
			require.NoError(t, err)
			require.NoError(t, mgr.Initialize(t.Context()))
			require.NoError(t, mgr.StartFileWatcher(t.Context()), "nothing to watch")
			require.NoError(t, mgr.CheckAndReindexChangedFiles(t.Context()), "nothing to re-index")

			results, _, err := mgr.Query(t.Context(), "cat")
			require.NoError(t, err)
			require.NotEmpty(t, results)
			assert.Equal(t, "pets/cats.md", results[0].Document.SourcePath, "documents keep the host's logical path")
			assert.NotContains(t, sourcePaths(results), "notes/todo.md", "docs: [pets] selects the pets directory only")
			assert.Contains(t, results[0].Document.Content, "Cats purr")
		})
	}
}

func TestDocumentsSelection(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		docs      string
		wantErr   string
		wantPaths []string
	}{
		"exact":        {docs: "[pets/cats.md]", wantPaths: []string{"pets/cats.md"}},
		"directory":    {docs: "[pets/]", wantPaths: []string{"pets/cats.md", "pets/dogs.md"}},
		"root":         {docs: "[.]", wantPaths: []string{"notes/todo.md", "pets/cats.md", "pets/dogs.md"}},
		"glob":         {docs: "['**/*.md']", wantPaths: []string{"notes/todo.md", "pets/cats.md", "pets/dogs.md"}},
		"per strategy": {docs: "[]", wantPaths: []string{"notes/todo.md"}},
		"missing":      {docs: "[pets/birds.md]", wantErr: fmt.Sprintf("%q selects none of the supplied documents [notes/todo.md pets/cats.md pets/dogs.md]", filepath.FromSlash("/pets/birds.md"))},
		"bad glob":     {docs: "['pets/[']", wantErr: "invalid glob pattern"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ragYAML := `
docs: ` + tc.docs + `
strategies:
  - type: bm25
    database: $TMP/bm25.db
`
			if name == "per strategy" {
				ragYAML += "    docs: [notes/todo.md]\n"
			}
			mgr, err := newDocumentsManager(t, ragYAML, &keywordModel{}, testDocuments)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			bound, ok := mgr.strategies["bm25"].(*documentStrategy)
			require.True(t, ok)
			assert.ElementsMatch(t, tc.wantPaths, slices.Collect(maps.Keys(bound.docs)))
		})
	}
}

func TestSelectDocuments(t *testing.T) {
	t.Parallel()
	docs := Documents{"/abs/pets/./cats.md": []byte("cat"), "notes/todo.md": []byte("todo")}
	for name, tc := range map[string]struct {
		parentDir string
		docPaths  []string
		docs      Documents
		wantErr   string
		wantKeys  []string
	}{
		"root selects everything":     {parentDir: "/", docPaths: []string{"/"}, docs: docs, wantKeys: []string{"/abs/pets/./cats.md", "notes/todo.md"}},
		"unclean absolute key":        {parentDir: "/", docPaths: []string{"/abs/pets"}, docs: docs, wantKeys: []string{"/abs/pets/./cats.md"}},
		"sibling prefix is not a dir": {parentDir: "/", docPaths: []string{"/abs/pet"}, docs: docs, wantErr: "selects none"},
		"bad glob with no documents":  {parentDir: "/", docPaths: []string{"/pets/["}, docs: Documents{}, wantErr: "invalid glob pattern"},
		"missing with no documents":   {parentDir: "/", docPaths: []string{"/pets"}, docs: Documents{}, wantErr: `"/pets" selects none of the supplied documents []`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			selected, err := SelectDocuments(tc.parentDir, tc.docPaths, tc.docs)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.ElementsMatch(t, tc.wantKeys, slices.Collect(maps.Keys(selected)))
		})
	}
}

func TestDocumentsRejectExplicitRespectVCS(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		ragYAML string
		wantErr string
	}{
		"top-level true": {
			ragYAML: "docs: [pets]\nrespect_vcs: true\nstrategies:\n  - type: bm25\n    database: $TMP/bm25.db\n",
			wantErr: `RAG "pets": respect_vcs: true does not apply to supplied documents`,
		},
		"strategy true": {
			ragYAML: "docs: [pets]\nstrategies:\n  - type: bm25\n    database: $TMP/bm25.db\n    respect_vcs: true\n",
			wantErr: `RAG "pets": strategy bm25: respect_vcs: true does not apply to supplied documents`,
		},
		"explicit false": {
			ragYAML: "docs: [pets]\nrespect_vcs: false\nstrategies:\n  - type: bm25\n    database: $TMP/bm25.db\n    respect_vcs: false\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := newDocumentsManager(t, tc.ragYAML, &keywordModel{}, testDocuments)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tc.wantErr)
		})
	}
}

func TestDocumentsAreIndexedOnce(t *testing.T) {
	t.Parallel()
	var model keywordModel
	mgr, err := newDocumentsManager(t, `
docs: [pets]
strategies:
  - type: chunked-embeddings
    database: $TMP/chunked.db
    embedding_model: mock/keyword
    vector_dimensions: 3
`, &model, testDocuments)
	require.NoError(t, err)
	require.NoError(t, mgr.Initialize(t.Context()))
	indexed := model.embeddings.Load()
	assert.Equal(t, int64(2), indexed, "one embedding per document chunk")

	require.NoError(t, mgr.Initialize(t.Context()))
	assert.Equal(t, indexed, model.embeddings.Load(), "unchanged documents are not re-embedded")
}

func TestDocumentsBackFullContent(t *testing.T) {
	t.Parallel()
	docs := Documents{"long.md": []byte(strings.Repeat("cat ", 100) + "\n" + strings.Repeat("dog ", 100))}
	mgr, err := newDocumentsManager(t, `
docs: [long.md]
strategies:
  - type: bm25
    database: $TMP/bm25.db
    chunking:
      size: 120
results:
  return_full_content: true
  deduplicate: true
`, &keywordModel{}, docs)
	require.NoError(t, err)
	require.NoError(t, mgr.Initialize(t.Context()))

	results, _, err := mgr.Query(t.Context(), "dog")
	require.NoError(t, err)
	require.Len(t, results, 1, "chunks of the same document collapse into one full document")
	assert.Equal(t, string(docs["long.md"]), results[0].Document.Content)
}

func TestDocumentsAreSnapshottedAtBuild(t *testing.T) {
	t.Parallel()
	content := []byte("cats purr")
	docs := Documents{"cats.md": content}
	mgr, err := newDocumentsManager(t, `
docs: [cats.md]
strategies:
  - type: bm25
    database: $TMP/bm25.db
results:
  return_full_content: true
`, &keywordModel{}, docs)
	require.NoError(t, err)

	copy(content, "dogs bark")
	delete(docs, "cats.md")
	docs["dogs.md"] = []byte("dogs bark")

	require.NoError(t, mgr.Initialize(t.Context()))
	results, _, err := mgr.Query(t.Context(), "cats")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "cats.md", results[0].Document.SourcePath)
	assert.Equal(t, "cats purr", results[0].Document.Content, "full content comes from the snapshot, not the mutated bytes")
}

type closingStrategy struct {
	staticStrategy

	closed bool
}

func (s *closingStrategy) Close() error {
	s.closed = true
	return nil
}

type closingIndexer struct{ closingStrategy }

func (s *closingIndexer) InitializeDocuments(context.Context, map[string][]byte, strategy.ChunkingConfig) error {
	return nil
}

func TestBindDocumentStrategiesClosesBuiltStrategiesOnError(t *testing.T) {
	t.Parallel()
	bound := &closingIndexer{}
	plain := &closingStrategy{}
	err := bindDocumentStrategies([]strategy.Config{
		{Name: "ok", Strategy: bound, Docs: []string{"/pets"}},
		{Name: "static", Strategy: plain},
	}, "/", testDocuments)
	require.EqualError(t, err, "strategy static cannot index supplied documents")
	assert.True(t, bound.closed, "already bound strategies are closed")
	assert.True(t, plain.closed, "the failing strategy is closed")
}

func TestDocumentsRejectStrategiesThatCannotIndexThem(t *testing.T) {
	t.Parallel()
	_, err := newDocumentStrategy(strategy.Config{Name: "static", Strategy: &staticStrategy{}}, "/", testDocuments)
	require.EqualError(t, err, "strategy static cannot index supplied documents")
}
