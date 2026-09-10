package rag

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/rag"
	ragtypes "github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tools"
)

// CreateToolSet is used by the tools registry.
func CreateToolSet(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig) (tools.ToolSet, error) {
	if toolset.RAGConfig == nil {
		return nil, errors.New("rag toolset requires either a rag_config block or a ref")
	}

	ragName := cmp.Or(toolset.Name, "rag")

	mgr, err := rag.NewManager(ctx, ragName, toolset.RAGConfig, rag.ManagersBuildConfig{
		ParentDir:     parentDir,
		ModelsGateway: runConfig.ModelsGateway,
		Env:           runConfig.EnvProvider(),
		Models:        runConfig.Models,
		Providers:     runConfig.Providers,
		RuntimeConfig: runConfig,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create RAG manager: %w", err)
	}

	toolName := cmp.Or(mgr.ToolName(), ragName)
	return New(mgr, toolName, WithIndexingTimeout(toolset.RAGConfig.GetIndexingTimeout())), nil
}

// EventCallback is called to forward RAG manager events during initialization.
type EventCallback = ragtypes.EventCallback

// ToolSet provides document querying capabilities for a single RAG source.
type ToolSet struct {
	manager       *rag.Manager
	toolName      string
	eventCallback EventCallback
	cancelWatcher context.CancelFunc
	wg            sync.WaitGroup
	// indexingTimeout bounds a single Initialize call. Zero means unbounded.
	// Resolved once by the caller (config.RAGConfig.GetIndexingTimeout()); the
	// zero value here is a plain Go zero, not "apply the default".
	indexingTimeout time.Duration
}

// Verify interface compliance.
var (
	_ tools.ToolSet      = (*ToolSet)(nil)
	_ tools.Instructable = (*ToolSet)(nil)
	_ tools.Startable    = (*ToolSet)(nil)
)

// Option configures optional ToolSet behavior.
type Option func(*ToolSet)

// WithIndexingTimeout bounds a single Initialize call started by Start,
// independent of the caller's own (much shorter) start-wait budget. Zero
// (the default) means unbounded.
func WithIndexingTimeout(d time.Duration) Option {
	return func(t *ToolSet) {
		t.indexingTimeout = d
	}
}

// New creates a new RAG toolset for a single RAG manager.
func New(manager *rag.Manager, toolName string, opts ...Option) *ToolSet {
	t := &ToolSet{
		manager:  manager,
		toolName: toolName,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name returns the tool name for this RAG source.
func (t *ToolSet) Name() string {
	return t.toolName
}

// SetEventCallback sets a callback to receive RAG manager events during
// initialization. Must be called before Start().
func (t *ToolSet) SetEventCallback(cb EventCallback) {
	t.eventCallback = cb
}

// Start initializes the RAG manager (indexes documents) and starts a
// file watcher for incremental updates.
//
// Indexing is detached from the caller's cancellation, just like the file
// watcher: Start may run under a short-lived startup-probe context (the
// wait budget documented on tools.DefaultStartTimeout / TryStartWithTimeout),
// and a caller giving up on that budget must not abort in-flight work —
// TryStartWithTimeout already promises that an abandoned Start keeps running
// and is picked up by a later call once it completes. Indexing is instead
// bounded only by indexingTimeout (see WithIndexingTimeout; zero means
// unbounded) and by Stop.
func (t *ToolSet) Start(ctx context.Context) error {
	if t.manager == nil {
		return nil
	}

	// The watcher, event forwarder and indexing are all long-lived and owned
	// by Stop() via cancelWatcher: detach them from the caller's cancellation
	// while keeping its values (logging, tracing).
	watchCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	t.cancelWatcher = cancel

	// Forward RAG manager events if a callback is set.
	if t.eventCallback != nil {
		t.wg.Go(func() {
			t.forwardEvents(watchCtx)
		})
	}

	// initCtx bounds a single Initialize run: derived from the detached
	// watchCtx (so Stop still cancels it), plus indexingTimeout when set.
	initCtx := watchCtx
	if t.indexingTimeout > 0 {
		var initCancel context.CancelFunc
		initCtx, initCancel = context.WithTimeout(watchCtx, t.indexingTimeout)
		defer initCancel()
	}

	if err := t.manager.Initialize(initCtx); err != nil {
		cancel()
		t.wg.Wait()
		if errors.Is(initCtx.Err(), context.DeadlineExceeded) {
			slog.WarnContext(ctx, "RAG indexing exceeded indexing_timeout; progress is saved and resumes on the next start",
				"tool", t.toolName, "indexing_timeout", t.indexingTimeout)
			return fmt.Errorf("RAG manager %q: indexing exceeded indexing_timeout (%s); progress is saved and resumes on the next start: %w",
				t.toolName, t.indexingTimeout, err)
		}
		return fmt.Errorf("failed to initialize RAG manager %q: %w", t.toolName, err)
	}

	t.wg.Go(func() {
		if err := t.manager.StartFileWatcher(watchCtx); err != nil && !errors.Is(err, context.Canceled) {
			slog.ErrorContext(watchCtx, "Failed to start RAG file watcher", "tool", t.toolName, "error", err)
		}
	})
	return nil
}

// Stop closes the RAG manager and releases resources.
func (t *ToolSet) Stop(_ context.Context) error {
	if t.manager == nil {
		return nil
	}
	if t.cancelWatcher != nil {
		t.cancelWatcher()
	}
	t.wg.Wait()
	return t.manager.Close()
}

// forwardEvents reads events from the RAG manager and forwards them via the callback.
func (t *ToolSet) forwardEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-t.manager.Events():
			if !ok {
				return
			}
			t.eventCallback(event)
		}
	}
}

func (t *ToolSet) Instructions() string {
	if t.manager != nil {
		if instruction := t.manager.ToolInstruction(); instruction != "" {
			return instruction
		}
	}
	return fmt.Sprintf("Search documents in %s to find relevant code or documentation. "+
		"Provide a clear search query describing what you need.", t.toolName)
}

type queryRAGArgs struct {
	Query string `json:"query" jsonschema:"Search query"`
}

type queryResult struct {
	SourcePath string  `json:"source_path" jsonschema:"Path to the source document"`
	Content    string  `json:"content" jsonschema:"Relevant document chunk content"`
	Similarity float64 `json:"similarity" jsonschema:"Similarity score (0-1)"`
	ChunkIndex int     `json:"chunk_index" jsonschema:"Index of the chunk within the source document"`
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	var description string
	if t.manager != nil {
		description = t.manager.Description()
	}
	description = cmp.Or(description, fmt.Sprintf("Search project documents from %s to find relevant code or documentation. "+
		"Provide a natural language query describing what you need. "+
		"Returns the most relevant document chunks with file paths.", t.toolName))

	return []tools.Tool{{
		Name:         t.toolName,
		Category:     "knowledge",
		Description:  description,
		Parameters:   tools.MustSchemaFor[queryRAGArgs](),
		OutputSchema: tools.MustSchemaFor[[]queryResult](),
		Handler:      tools.NewHandler(t.handleQueryRAG),
		Annotations: tools.ToolAnnotations{
			ReadOnlyHint: true,
			Title:        "Query " + t.toolName,
		},
	}}, nil
}

func (t *ToolSet) handleQueryRAG(ctx context.Context, args queryRAGArgs) (*tools.ToolCallResult, error) {
	if args.Query == "" {
		return nil, errors.New("query cannot be empty")
	}

	results, usage, err := t.manager.Query(ctx, args.Query)
	if err != nil {
		return nil, fmt.Errorf("RAG query failed: %w", err)
	}

	if usage.TotalTokens > 0 || usage.Cost > 0 {
		model := usage.ModelID
		if model == "" {
			model = t.toolName // Fallback to tool name for strategies that do not specify a model ID (e.g. BM25)
		}
		telemetry.RecordTokenUsage(ctx, model, usage.TotalTokens, 0, usage.Cost)
	}

	out := make([]queryResult, 0, len(results))
	for _, r := range results {
		out = append(out, queryResult{
			SourcePath: r.Document.SourcePath,
			Content:    r.Document.Content,
			Similarity: r.Similarity,
			ChunkIndex: r.Document.ChunkIndex,
		})
	}

	slices.SortFunc(out, func(a, b queryResult) int {
		return cmp.Compare(b.Similarity, a.Similarity)
	})

	const maxResults = 10
	if len(out) > maxResults {
		out = out[:maxResults]
	}

	resultJSON, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal results: %w", err)
	}

	return tools.ResultSuccess(string(resultJSON)), nil
}
