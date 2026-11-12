//go:build js && wasm && docker_agent_wasm_rag

package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestBrowserRAGRegistration(t *testing.T) {
	registry := browserToolsets(nil)
	require.True(t, registry.Has("rag"))
	for name, tc := range map[string]struct {
		toolset latest.Toolset
		want    string
	}{
		"without config":    {latest.Toolset{Type: "rag"}, "requires a rag_config"},
		"docs not supplied": {latest.Toolset{Type: "rag", RAGConfig: &latest.RAGConfig{Docs: []string{"guide.md"}, Strategies: []latest.RAGStrategyConfig{{Type: "bm25"}}}}, `"/guide.md" selects none of the supplied documents []`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := registry.CreateTool(t.Context(), tc.toolset, "/", testRunConfig(), "")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestRAGRejectsWhatDocumentsCannotHonour(t *testing.T) {
	for name, tc := range map[string]struct {
		strategy string
		want     string
	}{
		"database":    {"database: rag.db", "strategies[0].database: nothing is persisted"},
		"code_aware":  {"chunking:\n      code_aware: true", "strategies[0].chunking.code_aware: tree-sitter needs cgo"},
		"respect_vcs": {"respect_vcs: true", "strategies[0].respect_vcs: there is no VCS"},
	} {
		t.Run(name, func(t *testing.T) {
			yaml := `
agents:
  root:
    model: mock/root
    toolsets:
      - type: rag
        ref: guide
rag:
  guide:
    docs: [guide.md]
    strategies:
      - type: bm25
        ` + strings.ReplaceAll(tc.strategy, "\n", "\n        ") + `
`
			h := testHost(&echoToolSet{}, map[string]provider.Provider{"root": newScriptedModel("mock/root")})
			_, err := h.openSession(t.Context(), sessionOptions{YAML: yaml, Documents: map[string][]byte{"guide.md": []byte("hi")}})
			require.ErrorContains(t, err, tc.want)
		})
	}
}

const ragAgentYAML = `
models:
  embed:
    provider: mock
    model: embed
agents:
  root:
    model: mock/root
    instruction: Answer from the handbook.
    toolsets:
      - type: rag
        ref: handbook
rag:
  handbook:
    docs: [handbook]
    strategies:
      - type: bm25
      - type: chunked-embeddings
        embedding_model: embed
        vector_dimensions: 3
        max_indexing_concurrency: 1
        threshold: 0
    results:
      limit: 2
`

var handbook = map[string][]byte{
	"handbook/leave.md":    []byte("Vacation: everyone gets 25 days of vacation per year. Vacation requests go to your manager."),
	"handbook/expenses.md": []byte("Expenses: file expense reports within 30 days with receipts attached."),
	"private/salaries.md":  []byte("Salaries are confidential."),
}

func TestRAGIndexesTheSessionDocuments(t *testing.T) {
	models := map[string]provider.Provider{
		"root":  newScriptedModel("mock/root", toolTurn("handbook", `{"query":"vacation days"}`), textTurn("25 days")),
		"embed": &keywordEmbedder{},
	}
	h := testHost(&echoToolSet{}, models)
	s := openTestSession(t, h, sessionOptions{YAML: ragAgentYAML, Documents: handbook})

	var c collectingEmitter
	_, err := s.send("how much vacation do I get?", c.emit)
	require.NoError(t, err)
	assert.Empty(t, c.find("tool_confirmation"), "the rag tool is read-only")
	results := c.find("tool_result")
	require.Len(t, results, 1)
	output := results[0]["output"].(string)
	assert.Contains(t, output, `"source_path":"handbook/leave.md"`, "results carry the host's logical paths")
	assert.Contains(t, output, "25 days of vacation")
	assert.NotContains(t, output, "salaries", "docs: [handbook] leaves the other documents out")
	assert.Positive(t, models["embed"].(*keywordEmbedder).calls.Load(), "the embedding strategy went through the mock model")

	models["root"] = newScriptedModel("mock/root", toolTurn("handbook", `{"query":"vacation"}`), textTurn("unlimited"))
	other := openTestSession(t, h, sessionOptions{YAML: strings.ReplaceAll(ragAgentYAML, "docs: [handbook]", "docs: [notes.md]"), Documents: map[string][]byte{"notes.md": []byte("vacation is unlimited")}})
	var c2 collectingEmitter
	_, err = other.send("vacation?", c2.emit)
	require.NoError(t, err)
	require.Len(t, c2.find("tool_result"), 1)
	assert.Contains(t, c2.find("tool_result")[0]["output"], "unlimited")
	assert.NotContains(t, c2.find("tool_result")[0]["output"], "25 days", "documents are scoped to their session")
}

func TestRAGExampleLoadsOverTheSessionDocuments(t *testing.T) {
	yaml, err := os.ReadFile("examples/handbook-rag.yaml")
	require.NoError(t, err)
	opts := sessionOptions{YAML: string(yaml), Env: map[string]string{"OPENROUTER_API_KEY": "k"}}
	for _, documents := range []map[string][]byte{nil, {}} {
		opts.Documents = documents
		_, err = browserHost.openSession(t.Context(), opts)
		require.ErrorContains(t, err, `"/handbook" selects none of the supplied documents []`, "without documents there is nothing to index")
	}

	opts.Documents = handbook
	s, err := browserHost.openSession(t.Context(), opts)
	require.NoError(t, err)
	require.NoError(t, s.close())
}

// keywordEmbedder is an offline embedding model: vectors count the words
// "vacation" and "expense" so related texts land close together.
type keywordEmbedder struct {
	calls atomic.Int64
}

func (e *keywordEmbedder) ID() modelsdev.ID        { return modelsdev.NewID("mock", "embed") }
func (e *keywordEmbedder) BaseConfig() base.Config { return base.Config{} }

func (e *keywordEmbedder) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, errors.New("embedding model only")
}

func (e *keywordEmbedder) CreateEmbedding(_ context.Context, text string) (*base.EmbeddingResult, error) { //nolint:unparam // provider.EmbeddingProvider signature
	e.calls.Add(1)
	lower := strings.ToLower(text)
	return &base.EmbeddingResult{
		Embedding:   []float64{float64(strings.Count(lower, "vacation")), float64(strings.Count(lower, "expense")), 0.1},
		TotalTokens: 1,
	}, nil
}
