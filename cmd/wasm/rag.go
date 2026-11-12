//go:build js && wasm && docker_agent_wasm_rag

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/rag"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	ragtool "github.com/docker/docker-agent/pkg/tools/builtin/rag"
)

func registerRAG(creators map[string]teamloader.ToolsetCreator, documents map[string][]byte) {
	creators["rag"] = ragCreator(documents)
}

// ragCreator is ragtool.CreateToolSet over the session's documents: the
// manager indexes them in memory instead of files and starts no watcher, and
// the toolset keeps its tool, events and start behaviour.
func ragCreator(input map[string][]byte) teamloader.ToolsetCreator {
	documents := rag.Documents(input)
	if documents == nil {
		documents = rag.Documents{} // nil would mean "read files"
	}
	return func(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
		if err := checkRAG(toolset, documents); err != nil {
			return nil, err
		}
		ragName := cmp.Or(toolset.Name, "rag")
		mgr, err := rag.NewManager(ctx, ragName, toolset.RAGConfig, rag.ManagersBuildConfig{
			ParentDir:     parentDir,
			ModelsGateway: runConfig.ModelsGateway,
			Env:           runConfig.EnvProvider(),
			Models:        runConfig.Models,
			Providers:     runConfig.Providers,
			RuntimeConfig: runConfig,
			Documents:     documents,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create RAG manager: %w", err)
		}
		return ragtool.New(mgr, cmp.Or(mgr.ToolName(), ragName), ragtool.WithIndexingTimeout(toolset.RAGConfig.GetIndexingTimeout())), nil
	}
}

// checkRAG rejects what the session documents cannot honour: docs that
// select none of them, a database (nothing persists), code-aware chunking
// (tree-sitter needs cgo) and VCS ignore files (there is no checkout).
func checkRAG(ts latest.Toolset, documents map[string][]byte) error {
	if ts.RAGConfig == nil {
		return errors.New("rag toolset requires a rag_config block")
	}
	var errs []error
	if cfg := ts.RAGConfig; cfg.RespectVCS != nil && *cfg.RespectVCS {
		errs = append(errs, errors.New("respect_vcs: there is no VCS checkout in the browser"))
	}
	checkDocs := func(loc string, docs []string) {
		if _, err := rag.SelectDocuments(browserWorkingDir, rag.GetAbsolutePaths(browserWorkingDir, docs), documents); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", loc, err))
		}
	}
	checkDocs("docs", ts.RAGConfig.Docs)
	for i, s := range ts.RAGConfig.Strategies {
		loc := fmt.Sprintf("strategies[%d]", i)
		checkDocs(loc+".docs", s.Docs)
		if !s.Database.IsEmpty() {
			errs = append(errs, fmt.Errorf("%s.database: nothing is persisted in the browser; documents are indexed in memory for the session", loc))
		}
		if s.Chunking.CodeAware {
			errs = append(errs, fmt.Errorf("%s.chunking.code_aware: tree-sitter needs cgo and is not available in the browser", loc))
		}
		if respectVCS, _ := s.Params["respect_vcs"].(bool); respectVCS {
			errs = append(errs, fmt.Errorf("%s.respect_vcs: there is no VCS checkout in the browser", loc))
		}
	}
	return errors.Join(errs...)
}
