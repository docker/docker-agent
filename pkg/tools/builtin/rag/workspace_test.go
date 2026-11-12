package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	"github.com/docker/docker-agent/pkg/tools"
)

type workspaceEmbedder struct {
	calls atomic.Int32
}

func (*workspaceEmbedder) ID() modelsdev.ID        { return modelsdev.NewID("mock", "embedding") }
func (*workspaceEmbedder) BaseConfig() base.Config { return base.Config{} }

func (m *workspaceEmbedder) CreateEmbedding(context.Context, string) (*base.EmbeddingResult, error) {
	m.calls.Add(1)
	return &base.EmbeddingResult{Embedding: []float64{0.1, 0.2}, TotalTokens: 1}, nil
}

func (*workspaceEmbedder) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, errors.New("chat completions are not supported")
}

func workspaceRuntimeConfig() (*config.RuntimeConfig, *workspaceEmbedder) {
	model := &workspaceEmbedder{}
	return &config.RuntimeConfig{
		EnvProviderOverride:    environment.NewMapEnvProvider(nil),
		ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{}),
		ProviderRegistry: provider.NewRegistry(map[string]provider.Factory{
			"mock": func(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (provider.Provider, error) {
				return model, nil
			},
		}),
	}, model
}

func TestCreateToolSetWorkspacePaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		workingDir   bool
		strategyDocs bool
		absoluteDocs bool
		absoluteDB   bool
	}{
		{name: "workspace shared docs", workingDir: true},
		{name: "workspace strategy docs", workingDir: true, strategyDocs: true},
		{name: "parent fallback"},
		{name: "absolute docs", workingDir: true, absoluteDocs: true},
		{name: "absolute database", workingDir: true, absoluteDB: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parentDir, workspaceDir := t.TempDir(), t.TempDir()
			for dir, content := range map[string]string{parentDir: "parent document", workspaceDir: "workspace document"} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "note.md"), []byte(content), 0o644))
			}
			rc, _ := workspaceRuntimeConfig()
			baseDir := parentDir
			if tc.workingDir {
				rc.WorkingDir = workspaceDir
				baseDir = workspaceDir
			}
			docPath, dbPath := "note.md", "rag.db"
			wantDoc, wantDB := filepath.Join(baseDir, docPath), filepath.Join(baseDir, dbPath)
			if tc.absoluteDocs {
				docPath = filepath.Join(parentDir, docPath)
				wantDoc = docPath
			}
			if tc.absoluteDB {
				dbPath = filepath.Join(parentDir, dbPath)
				wantDB = dbPath
			}
			var ragCfg latest.RAGConfig
			require.NoError(t, yaml.Unmarshal(fmt.Appendf(nil, `
respect_vcs: false
docs: [%q]
strategies:
  - type: chunked-embeddings
    embedding_model: mock/embedding
    vector_dimensions: 2
    database: %q
`, docPath, dbPath), &ragCfg))
			if tc.strategyDocs {
				ragCfg.Strategies[0].Docs = ragCfg.Docs
				ragCfg.Docs = nil
			}

			ts, err := Creator(t.Context(), latest.Toolset{Type: "rag", Name: "knowledge", RAGConfig: &ragCfg}, parentDir, rc, "agent")
			require.NoError(t, err)
			startable := ts.(tools.Startable)
			t.Cleanup(func() { assert.NoError(t, startable.Stop(t.Context())) })
			require.FileExists(t, wantDB)
			if wantDB != filepath.Join(parentDir, "rag.db") {
				require.NoFileExists(t, filepath.Join(parentDir, "rag.db"))
			}
			require.NoError(t, startable.Start(t.Context()))

			toolz, err := ts.Tools(t.Context())
			require.NoError(t, err)
			require.Len(t, toolz, 1)
			result, err := toolz[0].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"query":"document"}`}}, nil)
			require.NoError(t, err)
			require.False(t, result.IsError, result.Output)
			var matches []queryResult
			require.NoError(t, json.Unmarshal([]byte(result.Output), &matches))
			require.Len(t, matches, 1)
			assert.Equal(t, wantDoc, matches[0].SourcePath)
			content, err := os.ReadFile(wantDoc)
			require.NoError(t, err)
			assert.Equal(t, string(content), matches[0].Content)

			originalDB, err := ragCfg.Strategies[0].Database.AsString()
			require.NoError(t, err)
			assert.Equal(t, dbPath, originalDB, "building must not rewrite the reusable config")
			if tc.strategyDocs {
				assert.Equal(t, []string{docPath}, ragCfg.Strategies[0].Docs)
			} else {
				assert.Equal(t, []string{docPath}, ragCfg.Docs)
			}
		})
	}
}

func TestCreateToolSetSharesOnlyWithinWorkspace(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		t.Run(fmt.Sprintf("explicit database=%t", explicit), func(t *testing.T) {
			t.Parallel()
			parentDir, workspaceDir := t.TempDir(), t.TempDir()
			for dir, content := range map[string]string{parentDir: "parent document", workspaceDir: "workspace document"} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "note.md"), []byte(content), 0o644))
			}
			var ragCfg latest.RAGConfig
			require.NoError(t, yaml.Unmarshal([]byte(`
respect_vcs: false
docs: [note.md]
strategies:
  - type: chunked-embeddings
    embedding_model: mock/embedding
    vector_dimensions: 2
    database: rag.db
`), &ragCfg))
			if !explicit {
				ragCfg.Strategies[0].Database = latest.RAGDatabaseConfig{}
			}
			rc, model := workspaceRuntimeConfig()
			var refs []*sharedToolSet
			for _, wd := range []string{parentDir, workspaceDir, workspaceDir} {
				workspaceRC := rc.Clone()
				workspaceRC.WorkingDir = wd
				ts, err := CreateToolSet(t.Context(), latest.Toolset{Name: "knowledge", RAGConfig: &ragCfg}, parentDir, workspaceRC)
				require.NoError(t, err)
				ref := ts.(*sharedToolSet)
				t.Cleanup(func() { assert.NoError(t, ref.Stop(t.Context())) })
				refs = append(refs, ref)
			}
			require.False(t, sameKey(refs[0].key, refs[1].key), "parent and workspace indexes must not share")
			require.True(t, sameKey(refs[1].key, refs[2].key), "references within one workspace still share")
			for _, ref := range refs {
				require.NoError(t, ref.Start(t.Context()))
			}
			assert.NotSame(t, refs[0].currentEntry(), refs[1].currentEntry())
			assert.Same(t, refs[1].currentEntry(), refs[2].currentEntry())
			assert.Equal(t, int32(2), model.calls.Load(), "one document is indexed per workspace")

			for i, ref := range refs {
				result, err := ref.target().handleQueryRAG(t.Context(), queryRAGArgs{Query: "document"})
				require.NoError(t, err)
				var matches []queryResult
				require.NoError(t, json.Unmarshal([]byte(result.Output), &matches))
				require.Len(t, matches, 1)
				want := "workspace document"
				if i == 0 {
					want = "parent document"
				}
				assert.Equal(t, want, matches[0].Content)
			}
		})
	}
}
