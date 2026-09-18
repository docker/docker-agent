package rag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/rag/strategy"
)

// Documents are documents supplied in memory, keyed by logical path. A
// manager built with them indexes the documents its configured docs select
// instead of reading the filesystem, and never watches for changes.
type Documents map[string][]byte

func (d Documents) clone() Documents {
	if d == nil {
		return nil
	}
	c := make(Documents, len(d))
	for path, content := range d {
		c[path] = slices.Clone(content)
	}
	return c
}

// validateDocumentsConfig rejects settings that need the filesystem. The
// respect_vcs default (nil) is silently off for supplied documents; an
// explicit true, at either level, asks for .gitignore lookups that cannot
// apply.
func validateDocumentsConfig(ragCfg *latest.RAGConfig) error {
	if ragCfg.RespectVCS != nil && *ragCfg.RespectVCS {
		return errors.New("respect_vcs: true does not apply to supplied documents")
	}
	for _, sc := range ragCfg.Strategies {
		if v, ok := sc.Params["respect_vcs"].(bool); ok && v {
			return fmt.Errorf("strategy %s: respect_vcs: true does not apply to supplied documents", sc.Type)
		}
	}
	return nil
}

// documentStrategy binds a strategy to the supplied documents its doc paths
// selected at build time. Documents never change, so change checks and the
// file watcher do nothing; queries go to the wrapped strategy.
type documentStrategy struct {
	strategy.Strategy

	indexer strategy.DocumentIndexer
	docs    map[string][]byte
}

// bindDocumentStrategies replaces every built strategy with one bound to the
// documents its docs select. On failure the built strategies are closed so
// their databases do not leak.
func bindDocumentStrategies(strategyConfigs []strategy.Config, parentDir string, docs Documents) error {
	for i := range strategyConfigs {
		bound, err := newDocumentStrategy(strategyConfigs[i], parentDir, docs)
		if err != nil {
			for _, sc := range strategyConfigs {
				if closeErr := sc.Strategy.Close(); closeErr != nil {
					slog.Warn("Failed to close strategy", "strategy", sc.Name, "error", closeErr)
				}
			}
			return err
		}
		strategyConfigs[i].Strategy = bound
	}
	return nil
}

func newDocumentStrategy(cfg strategy.Config, parentDir string, docs Documents) (*documentStrategy, error) {
	indexer, ok := cfg.Strategy.(strategy.DocumentIndexer)
	if !ok {
		return nil, fmt.Errorf("strategy %s cannot index supplied documents", cfg.Name)
	}
	selected, err := SelectDocuments(parentDir, cfg.Docs, docs)
	if err != nil {
		return nil, fmt.Errorf("docs: %w", err)
	}
	return &documentStrategy{Strategy: cfg.Strategy, indexer: indexer, docs: selected}, nil
}

func (d *documentStrategy) Initialize(ctx context.Context, _ []string, chunking strategy.ChunkingConfig) error {
	return d.indexer.InitializeDocuments(ctx, d.docs, chunking)
}

func (d *documentStrategy) CheckAndReindexChangedFiles(context.Context, []string, strategy.ChunkingConfig) error {
	return nil
}

func (d *documentStrategy) StartFileWatcher(context.Context, []string, strategy.ChunkingConfig) error {
	return nil
}

// SelectDocuments resolves docPaths, already made absolute against parentDir
// (see GetAbsolutePaths), against the documents' paths made absolute the same
// way. Like a filesystem doc entry, a path selects the document at that path,
// every document under that directory, or the documents matching it as a
// glob. A path selecting nothing is an error: the config asks for documents
// the host did not supply. Hosts can call it to validate a config up front.
func SelectDocuments(parentDir string, docPaths []string, docs Documents) (map[string][]byte, error) {
	selected := make(map[string][]byte)
	for _, docPath := range docPaths {
		matches, err := documentMatcher(docPath)
		if err != nil {
			return nil, err
		}
		matched := false
		for key, content := range docs {
			if matches(filepath.Clean(GetAbsolutePaths(parentDir, []string{key})[0])) {
				selected[key] = content
				matched = true
			}
		}
		if !matched {
			return nil, fmt.Errorf("%q selects none of the supplied documents %v", docPath, slices.Sorted(maps.Keys(docs)))
		}
	}
	return selected, nil
}

// documentMatcher reports whether a clean absolute document path is selected
// by docPath: as a glob (same detection as fsx.CollectFiles), or as the path
// itself or a directory containing it.
func documentMatcher(docPath string) (func(path string) bool, error) {
	if strings.ContainsAny(docPath, "*?[") {
		if !doublestar.ValidatePathPattern(docPath) {
			return nil, fmt.Errorf("invalid glob pattern %q: %w", docPath, doublestar.ErrBadPattern)
		}
		return func(path string) bool {
			match, _ := doublestar.PathMatch(docPath, path)
			return match
		}, nil
	}
	docPath = filepath.Clean(docPath)
	dir := docPath
	if !strings.HasSuffix(dir, string(filepath.Separator)) {
		dir += string(filepath.Separator) // Clean("/") is already "/"; "//" would match nothing
	}
	return func(path string) bool {
		return path == docPath || strings.HasPrefix(path, dir)
	}, nil
}
