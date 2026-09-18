package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/deferred"
	"github.com/docker/docker-agent/pkg/tools/codemode"
)

// toolCapturingProvider records the tools of every request it receives and
// exposes a configurable model config so BaseConfig gating can be exercised.
type toolCapturingProvider struct {
	cfg    latest.ModelConfig
	stream chat.MessageStream
	err    error

	mu       sync.Mutex
	requests [][]tools.Tool
}

func (p *toolCapturingProvider) ID() modelsdev.ID {
	return modelsdev.NewID(p.cfg.Provider, p.cfg.Model)
}

func (p *toolCapturingProvider) CreateChatCompletionStream(_ context.Context, _ []chat.Message, requestTools []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	p.requests = append(p.requests, requestTools)
	p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	return p.stream, nil
}

func (p *toolCapturingProvider) BaseConfig() base.Config { return base.Config{ModelConfig: p.cfg} }

func (p *toolCapturingProvider) requestNames() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([][]string, 0, len(p.requests))
	for _, request := range p.requests {
		names = append(names, toolNames(request))
	}
	return names
}

func nativeSearchConfig() latest.ModelConfig {
	return latest.ModelConfig{Provider: "openai", Model: "gpt-5.4", ProviderOpts: map[string]any{"native_tool_search": true}}
}

func legacyConfig() latest.ModelConfig {
	return latest.ModelConfig{Provider: "openai", Model: "gpt-5.4"}
}

var noopHandler = func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
	return tools.ResultSuccess("ok"), nil
}

// newDeferredAgentToolSets mirrors the loader: the source's tools are hidden
// from the regular list and only reachable through the deferred toolset.
func newDeferredAgentToolSets(sourceTools ...tools.Tool) []tools.ToolSet {
	source := newStubToolSet(nil, sourceTools, nil)
	dt := deferred.New()
	dt.AddSource(source, true, nil)
	return []tools.ToolSet{newStubToolSet(nil, []tools.Tool{{Name: "regular", Handler: noopHandler}}, nil), dt}
}

func getToolsFor(t *testing.T, sess *session.Session, toolsets []tools.ToolSet, models ...*toolCapturingProvider) []tools.Tool {
	t.Helper()
	opts := []agent.Opt{agent.WithToolSets(toolsets...), agent.WithModel(models[0])}
	for _, fallback := range models[1:] {
		opts = append(opts, agent.WithFallbackModel(fallback))
	}
	root := agent.New("root", "test", opts...)
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	got, err := rt.getTools(t.Context(), sess, root, trace.SpanFromContext(t.Context()), NewChannelSink(make(chan Event, 10)), false)
	require.NoError(t, err)
	return got
}

func TestGetTools_CatalogRequiresNativeToolSearch(t *testing.T) {
	t.Parallel()

	toolsets := newDeferredAgentToolSets(tools.Tool{Name: "write_file", Handler: noopHandler})

	t.Run("legacy model keeps search_tool/add_tool only", func(t *testing.T) {
		t.Parallel()
		got := getToolsFor(t, session.New(), toolsets, &toolCapturingProvider{cfg: legacyConfig()})
		assert.ElementsMatch(t, []string{"regular", deferred.ToolNameSearchTool, deferred.ToolNameAddTool}, toolNames(got))
	})

	t.Run("native model gets the catalog", func(t *testing.T) {
		t.Parallel()
		got := getToolsFor(t, session.New(), toolsets, &toolCapturingProvider{cfg: nativeSearchConfig()})
		assert.ElementsMatch(t, []string{"regular", deferred.ToolNameSearchTool, deferred.ToolNameAddTool, "write_file"}, toolNames(got))
		for _, tool := range got {
			assert.Equal(t, tool.Name == "write_file", tool.InCatalog, tool.Name)
			assert.Equal(t, tool.Name == "write_file", tool.SearchOnly, tool.Name)
			require.NotNil(t, tool.Handler, tool.Name)
		}
	})

	t.Run("native fallback model is enough", func(t *testing.T) {
		t.Parallel()
		got := getToolsFor(t, session.New(), toolsets, &toolCapturingProvider{cfg: legacyConfig()}, &toolCapturingProvider{cfg: nativeSearchConfig()})
		assert.Contains(t, toolNames(got), "write_file")
	})
}

func TestGetTools_CatalogHonoursSessionFilters(t *testing.T) {
	t.Parallel()

	toolsets := newDeferredAgentToolSets(
		tools.Tool{Name: "write_file", Handler: noopHandler},
		tools.Tool{Name: "read_file", Handler: noopHandler},
		tools.Tool{Name: "run_skill", Handler: noopHandler},
	)
	native := &toolCapturingProvider{cfg: nativeSearchConfig()}

	sess := session.New()
	sess.ExcludedTools = []string{"run_skill"}
	got := getToolsFor(t, sess, toolsets, native)
	assert.ElementsMatch(t, []string{"regular", deferred.ToolNameSearchTool, deferred.ToolNameAddTool, "write_file", "read_file"}, toolNames(got))

	sess = session.New()
	sess.AllowedTools = []string{"read_*", deferred.ToolNameSearchTool}
	got = getToolsFor(t, sess, toolsets, native)
	assert.ElementsMatch(t, []string{deferred.ToolNameSearchTool, "read_file"}, toolNames(got))
}

// Activating a catalog tool through add_tool keeps it InCatalog, so the
// native provider still declares it through tool search (the declaration is
// activation-independent), but it is no longer SearchOnly: a legacy fallback
// model in the same chain keeps calling it as a regular tool.
func TestGetTools_CatalogStableAfterAddTool(t *testing.T) {
	t.Parallel()

	dt := deferred.New()
	dt.AddSource(newStubToolSet(nil, []tools.Tool{{Name: "write_file", Handler: noopHandler}, {Name: "list_dir", Handler: noopHandler}}, nil), true, nil)
	toolsets := []tools.ToolSet{newStubToolSet(nil, []tools.Tool{{Name: "regular", Handler: noopHandler}}, nil), dt}
	native := &toolCapturingProvider{cfg: nativeSearchConfig()}
	legacy := &toolCapturingProvider{cfg: legacyConfig()}
	all := []string{"regular", deferred.ToolNameSearchTool, deferred.ToolNameAddTool, "list_dir", "write_file"}

	before := getToolsFor(t, session.New(), toolsets, native, legacy)
	assert.Equal(t, all, toolNames(before))
	assert.Equal(t, []string{"list_dir", "write_file"}, inCatalogNames(before))
	assert.Equal(t, []string{"list_dir", "write_file"}, searchOnlyNames(before))
	assert.Equal(t, all, toolNames(toolsForProvider(t.Context(), native, before)))
	assert.Equal(t, []string{"regular", deferred.ToolNameSearchTool, deferred.ToolNameAddTool}, toolNames(toolsForProvider(t.Context(), legacy, before)))

	result, err := callTool(t, before, deferred.ToolNameAddTool, map[string]any{"name": "write_file"})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "activated")

	// The activated tool now comes from the deferred toolset's regular
	// listing, ahead of the still-deferred catalog entries; the native
	// provider sorts its hosted declarations so the request stays byte-stable.
	after := getToolsFor(t, session.New(), toolsets, native, legacy)
	afterNames := []string{"regular", deferred.ToolNameSearchTool, deferred.ToolNameAddTool, "write_file", "list_dir"}
	assert.Equal(t, afterNames, toolNames(after))
	assert.Equal(t, []string{"write_file", "list_dir"}, inCatalogNames(after), "activation does not change the native declaration set")
	assert.Equal(t, []string{"list_dir"}, searchOnlyNames(after))
	assert.Equal(t, afterNames, toolNames(toolsForProvider(t.Context(), native, after)))
	assert.Equal(t, []string{"regular", deferred.ToolNameSearchTool, deferred.ToolNameAddTool, "write_file"}, toolNames(toolsForProvider(t.Context(), legacy, after)), "the activated tool stays callable on the legacy model")

	// Without a native model in the chain the legacy surface is unchanged:
	// activated tools are regular tools.
	legacyOnly := getToolsFor(t, session.New(), toolsets, legacy)
	assert.Equal(t, []string{"regular", deferred.ToolNameSearchTool, deferred.ToolNameAddTool, "write_file"}, toolNames(legacyOnly))
	assert.Empty(t, inCatalogNames(legacyOnly))
}

func inCatalogNames(ts []tools.Tool) []string {
	return namesWhere(ts, func(t tools.Tool) bool { return t.InCatalog })
}

func searchOnlyNames(ts []tools.Tool) []string {
	return namesWhere(ts, func(t tools.Tool) bool { return t.SearchOnly })
}

func namesWhere(ts []tools.Tool, keep func(tools.Tool) bool) []string {
	var names []string
	for _, tool := range ts {
		if keep(tool) {
			names = append(names, tool.Name)
		}
	}
	return names
}

// Code Mode hides its children behind run_tools_with_javascript, catalog
// included: hosted tool search must not surface them as direct tools.
func TestGetTools_CatalogNotExposedThroughCodeMode(t *testing.T) {
	t.Parallel()

	toolsets := []tools.ToolSet{codemode.Wrap(newDeferredAgentToolSets(tools.Tool{Name: "write_file", Parameters: map[string]any{"type": "object"}, Handler: noopHandler})...)}
	got := getToolsFor(t, session.New(), toolsets, &toolCapturingProvider{cfg: nativeSearchConfig()})
	assert.Equal(t, []string{"run_tools_with_javascript"}, toolNames(got))
}

func callTool(t *testing.T, agentTools []tools.Tool, name string, args map[string]any) (*tools.ToolCallResult, error) {
	t.Helper()
	idx := slices.IndexFunc(agentTools, func(tool tools.Tool) bool { return tool.Name == name })
	require.NotEqual(t, -1, idx, name)
	raw, err := json.Marshal(args)
	require.NoError(t, err)
	return agentTools[idx].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: name, Arguments: string(raw)}}, tools.NopRuntime{})
}

func TestToolsForProvider(t *testing.T) {
	t.Parallel()

	agentTools := []tools.Tool{{Name: "regular"}, {Name: "activated", InCatalog: true}, {Name: "catalog", InCatalog: true, SearchOnly: true}}

	assert.Equal(t, agentTools, toolsForProvider(t.Context(), &toolCapturingProvider{cfg: nativeSearchConfig()}, agentTools))
	assert.Equal(t, []string{"regular", "activated"}, toolNames(toolsForProvider(t.Context(), &toolCapturingProvider{cfg: legacyConfig()}, agentTools)))
	assert.Equal(t, []string{"regular", "activated"}, toolNames(toolsForProvider(t.Context(), &mockProvider{}, agentTools)))
}

// The fallback chain shares one tool list; each attempt must see only what
// its provider can handle: the native primary the full catalog, the legacy
// fallback the search_tool/add_tool surface.
func TestFallback_DropsCatalogForLegacyModel(t *testing.T) {
	t.Parallel()

	primary := &toolCapturingProvider{cfg: nativeSearchConfig(), err: errors.New("400 bad request")}
	fallback := &toolCapturingProvider{cfg: legacyConfig(), stream: newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build()}

	root := agent.New("root", "test",
		agent.WithToolSets(newDeferredAgentToolSets(tools.Tool{Name: "write_file", Handler: noopHandler})...),
		agent.WithModel(primary),
		agent.WithFallbackModel(fallback),
		agent.WithFallbackRetries(-1),
	)
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("hi"))
	for range rt.RunStream(t.Context(), sess) {
	}

	primaryRequests := primary.requestNames()
	require.Len(t, primaryRequests, 1)
	assert.Contains(t, primaryRequests[0], "write_file")

	fallbackRequests := fallback.requestNames()
	require.Len(t, fallbackRequests, 1)
	assert.ElementsMatch(t, []string{"regular", deferred.ToolNameSearchTool, deferred.ToolNameAddTool}, fallbackRequests[0])
}
