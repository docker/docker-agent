package teamloader

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/deferred"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
	"github.com/docker/docker-agent/pkg/tools/toon"
)

// slowSourceToolSet behaves like an MCP toolset: Tools fails with
// ErrNotStarted until Start completes, and Start blocks until released.
type slowSourceToolSet struct {
	starting chan struct{} // closed when Start is entered
	release  chan struct{}
	started  atomic.Bool
	once     sync.Once
}

func (s *slowSourceToolSet) Tools(context.Context) ([]tools.Tool, error) {
	if !s.started.Load() {
		return nil, lifecycle.ErrNotStarted
	}
	return []tools.Tool{{
		Name:        "remote_echo",
		Description: "Echoes its input",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess(`{"echo":"hi"}`), nil
		},
	}}, nil
}

func (s *slowSourceToolSet) Start(ctx context.Context) error {
	s.once.Do(func() { close(s.starting) })
	select {
	case <-s.release:
		s.started.Store(true)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *slowSourceToolSet) Stop(context.Context) error { return nil }

func namesOf(ts []tools.Tool) []string {
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.Name)
	}
	return names
}

func callTool(t *testing.T, ts []tools.Tool, name string, args any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	require.NoError(t, err)
	for _, tool := range ts {
		if tool.Name == name {
			res, err := tool.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: name, Arguments: string(raw)}}, tools.NopRuntime{})
			require.NoError(t, err)
			return res.Output
		}
	}
	t.Fatalf("tool %q not found in %v", name, namesOf(ts))
	return ""
}

// Reproduces the original bug end to end through the real loader pipeline: a
// message arrives (agent.Tools) while a deferred MCP-like source is still
// starting. The deferred toolset must come up without a warning, and the
// source's tools must become discoverable once the source is ready.
func TestDeferredToolsSurviveSlowSourceStart(t *testing.T) {
	t.Parallel()

	slow := &slowSourceToolSet{starting: make(chan struct{}), release: make(chan struct{})}
	registry := NewToolsetRegistry(map[string]ToolsetCreator{
		"slow": func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
			return slow, nil
		},
	})
	a := &latest.AgentConfig{
		Instruction: "test",
		Toolsets: []latest.Toolset{{
			Type:  "slow",
			Defer: latest.DeferConfig{DeferAll: true},
			Toon:  "remote_.*",
		}},
	}
	runConfig := config.RuntimeConfig{EnvProviderForTests: &noEnvProvider{}}
	toolSets, warnings, err := getToolsForAgent(t.Context(), a, ".", &runConfig, "test-config", &loadOptions{toolsetRegistry: registry, toon: toon.Wrap, newDeferred: func() DeferredToolSet { return deferred.New() }}, js.NewJsExpander(runConfig.EnvProvider()))
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.Len(t, toolSets, 2, "source (with all tools hidden) + deferred aggregator")
	_, isDeferred := toolSets[1].(*deferred.ToolSet)
	require.True(t, isDeferred)

	ag := agent.New("root", "test", agent.WithToolSets(toolSets...))

	// Like the runtime's startup probe: kick off the source's start in the
	// background so that it is in flight when the first message arrives.
	sourceStartable, ok := ag.ToolSets()[0].(*tools.StartableToolSet)
	require.True(t, ok)
	go func() {
		_, _ = sourceStartable.TryStartWithTimeout(t.Context(), time.Minute)
	}()
	select {
	case <-slow.starting:
	case <-time.After(5 * time.Second):
		t.Fatal("source start never began")
	}

	// Turn 1: the source's Start is in flight (blocked); the turn must skip
	// it rather than wait, must not warn, and must still offer search_tool/add_tool.
	turn1 := make(chan []tools.Tool, 1)
	go func() {
		got, err := ag.Tools(t.Context())
		assert.NoError(t, err)
		turn1 <- got
	}()
	var got []tools.Tool
	select {
	case got = <-turn1:
	case <-time.After(10 * time.Second):
		t.Fatal("turn 1 blocked on the slow source start")
	}
	assert.ElementsMatch(t, []string{deferred.ToolNameSearchTool, deferred.ToolNameAddTool}, namesOf(got))
	assert.Empty(t, ag.DrainWarnings(), "deferred toolset must not fail to start while its source is starting")

	// While the source is still starting, its tools are simply not discoverable yet.
	out := callTool(t, got, deferred.ToolNameSearchTool, deferred.SearchToolArgs{Query: "echo"})
	assert.Contains(t, out, "No deferred tools found")

	// Source finishes starting; the abandoned Start goroutine completes.
	close(slow.release)
	require.Eventually(t, slow.started.Load, 5*time.Second, 10*time.Millisecond)

	// Turn 2: the source is now discoverable and activatable.
	require.Eventually(t, func() bool {
		got, err := ag.Tools(t.Context())
		require.NoError(t, err)
		return strings.Contains(callTool(t, got, deferred.ToolNameSearchTool, deferred.SearchToolArgs{Query: "echo"}), "remote_echo")
	}, 5*time.Second, 20*time.Millisecond)

	got, err = ag.Tools(t.Context())
	require.NoError(t, err)
	out = callTool(t, got, deferred.ToolNameAddTool, deferred.AddToolArgs{Name: "remote_echo"})
	assert.Contains(t, out, "has been activated")

	// Turn 3: the activated tool is listed, and runs through the source's
	// wrappers (TOON was configured for it).
	got, err = ag.Tools(t.Context())
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{deferred.ToolNameSearchTool, deferred.ToolNameAddTool, "remote_echo"}, namesOf(got))
	assert.Equal(t, "echo: hi", callTool(t, got, "remote_echo", nil))
	assert.Empty(t, ag.DrainWarnings())
}
