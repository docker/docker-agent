package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

// toolListToolset is a minimal ToolSet that reports a fixed list of tools and a
// description. It implements neither Statable nor Startable, so its lifecycle
// state is driven purely by the StartableToolSet wrapper (started vs not) — the
// same path the built-in filesystem/shell toolsets take.
type toolListToolset struct {
	desc  string
	names []string
}

func (s *toolListToolset) Tools(context.Context) ([]tools.Tool, error) {
	out := make([]tools.Tool, 0, len(s.names))
	for _, n := range s.names {
		out = append(out, tools.Tool{Name: n})
	}
	return out, nil
}

func (s *toolListToolset) Describe() string { return s.desc }

// TestAgentToolsetStatuses verifies the named-agent lifecycle accessor mirrors
// CurrentAgentToolsetStatuses: it maps each toolset's live state (ready,
// stopped, failed + error/restart count) in declaration order, and yields nil
// for an unknown agent.
func TestAgentToolsetStatuses(t *testing.T) {
	t.Parallel()

	boom := errors.New("kaboom")
	ready := &statefulToolset{desc: "ready-ts", info: lifecycle.StateInfo{State: lifecycle.StateReady}}
	stopped := &statefulToolset{desc: "stopped-ts", info: lifecycle.StateInfo{State: lifecycle.StateStopped}}
	failed := &statefulToolset{desc: "failed-ts", info: lifecycle.StateInfo{State: lifecycle.StateFailed, LastError: boom, RestartCount: 2}}

	root := agent.New("root", "", agent.WithToolSets(ready, stopped, failed))
	tm := team.New(team.WithAgents(root))
	r := &LocalRuntime{team: tm, agents: newAgentRouter(tm, "root")}

	statuses := r.AgentToolsetStatuses("root")
	require.Len(t, statuses, 3)
	assert.Equal(t, lifecycle.StateReady, statuses[0].State)
	assert.Equal(t, lifecycle.StateStopped, statuses[1].State)
	assert.Equal(t, lifecycle.StateFailed, statuses[2].State)
	require.ErrorIs(t, statuses[2].LastError, boom)
	assert.Equal(t, 2, statuses[2].RestartCount)

	assert.Nil(t, r.AgentToolsetStatuses("missing"), "unknown agent yields nil")
}

// TestAgentConfigInfo_Inspector exercises the full inspector dataset: the
// static config (sub-agents, handoffs, fallbacks, skills, limits, option flags)
// combined with live toolset state. The filesystem toolset is started so it
// reports live tool names; git stays stopped and must fall back to its declared
// allow-list. The instruction is never surfaced.
func TestAgentConfigInfo_Inspector(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	prov := &mockProvider{id: "anthropic/claude-opus-4-8"}

	sub := agent.New("coder", "")
	hand := agent.New("planner", "")

	fsTS := tools.WithName(&toolListToolset{desc: "fs", names: []string{"read_file", "write_file"}}, "filesystem")
	gitTS := tools.WithName(&toolListToolset{desc: "git"}, "git")

	root := agent.New("root", "secret system instruction",
		agent.WithModel(prov),
		agent.WithFallbackModel(prov),
		agent.WithSubAgents(sub),
		agent.WithHandoffs(hand),
		agent.WithToolSets(fsTS, gitTS),
		agent.WithMaxIterations(50),
		agent.WithNumHistoryItems(40),
		agent.WithMaxConsecutiveToolCalls(5),
		agent.WithAddDate(true),
		agent.WithRedactSecrets(true),
	)

	cfg := latest.AgentConfig{
		Name:          "root",
		CodeModeTools: true,
		UseSkills:     []string{"code-review"},
		Skills: latest.SkillsConfig{
			Include: []string{"debugging"},
			Inline:  []latest.InlineSkill{{Name: "refactor"}},
		},
		Toolsets: []latest.Toolset{
			{Type: "filesystem", Tools: []string{"read_file", "write_file", "edit_file"}},
			{Type: "git", Tools: []string{"status", "commit"}},
		},
	}

	tm := team.New(
		team.WithAgents(root, sub, hand),
		team.WithAgentConfigs(map[string]latest.AgentConfig{"root": cfg}),
	)
	r := &LocalRuntime{team: tm, agents: newAgentRouter(tm, "root")}

	started := root.ToolSets()[0].(*tools.StartableToolSet)
	require.NoError(t, started.Start(ctx))

	got := r.AgentConfigInfo(t.Context(), "root")

	assert.Equal(t, []string{"coder"}, got.SubAgents)
	assert.Equal(t, []string{"planner"}, got.Handoffs)
	assert.Equal(t, []string{"anthropic/claude-opus-4-8"}, got.Fallbacks)
	assert.Equal(t, []string{"code-review", "debugging", "refactor"}, got.Skills)

	assert.Equal(t, 50, got.MaxIterations)
	assert.Equal(t, 40, got.NumHistoryItems)
	assert.Equal(t, 5, got.MaxConsecutiveToolCalls)

	assert.Equal(t, []string{"add-date", "redact-secrets", "code-mode-tools"}, got.Options)
	assert.True(t, got.IsCurrent)

	require.Len(t, got.Toolsets, 2)
	fs := got.Toolsets[0]
	assert.Equal(t, "filesystem", fs.Name)
	assert.Equal(t, ToolsetStarted, fs.State)
	assert.Equal(t, []string{"read_file", "write_file"}, fs.Tools, "started toolset reports live tool names")

	git := got.Toolsets[1]
	assert.Equal(t, "git", git.Name)
	assert.Equal(t, ToolsetStopped, git.State)
	assert.Equal(t, []string{"status", "commit"}, git.Tools, "stopped toolset reports its declared allow-list")
}

// blockingNamedToolSet is a minimal Startable toolset whose Start blocks
// until release is closed, modeling a toolset whose Start legitimately runs
// for a long time in the background (e.g. RAG indexing a large knowledge
// base, #4073).
type blockingNamedToolSet struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingNamedToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (b *blockingNamedToolSet) Start(context.Context) error {
	close(b.entered)
	<-b.release
	return nil
}
func (b *blockingNamedToolSet) Stop(context.Context) error { return nil }

// TestAgentConfigInfo_ToolsetMidStartReportsStartingWithDeclaredTools pins
// the #4073 fix end-to-end: while a toolset's Start is still running,
// AgentConfigInfo (and the AgentToolsetStatuses it builds on) must not block
// behind it, the lifecycle state must read as Starting rather than
// Ready/Stopped, and the declared `tools:` allow-list is reported since no
// live tool list exists yet.
func TestAgentConfigInfo_ToolsetMidStartReportsStartingWithDeclaredTools(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	prov := &mockProvider{id: "openai/gpt-5"}

	inner := &blockingNamedToolSet{entered: make(chan struct{}), release: make(chan struct{})}
	ragTS := tools.WithName(inner, "docs")

	root := agent.New("root", "", agent.WithModel(prov), agent.WithToolSets(ragTS))

	cfg := latest.AgentConfig{
		Name: "root",
		Toolsets: []latest.Toolset{
			{Type: "rag", Name: "docs", Tools: []string{"search_docs"}},
		},
	}

	tm := team.New(
		team.WithAgents(root),
		team.WithAgentConfigs(map[string]latest.AgentConfig{"root": cfg}),
	)
	r := &LocalRuntime{team: tm, agents: newAgentRouter(tm, "root")}

	startable := root.ToolSets()[0].(*tools.StartableToolSet)
	startDone := make(chan error, 1)
	go func() { startDone <- startable.Start(ctx) }()
	<-inner.entered

	infoDone := make(chan AgentConfigInfo, 1)
	go func() { infoDone <- r.AgentConfigInfo(ctx, "root") }()

	var got AgentConfigInfo
	select {
	case got = <-infoDone:
	case <-time.After(5 * time.Second):
		t.Fatal("AgentConfigInfo blocked behind an in-flight Start instead of reporting Starting")
	}

	require.Len(t, got.Toolsets, 1)
	docs := got.Toolsets[0]
	assert.Equal(t, "docs", docs.Name)
	assert.Equal(t, ToolsetStarted, docs.State, "the Starting lifecycle state buckets as started/serving, not stopped or error")
	assert.Equal(t, []string{"search_docs"}, docs.Tools, "mid-start toolset falls back to the declared allow-list, not an empty live list")

	statuses := r.AgentToolsetStatuses("root")
	require.Len(t, statuses, 1)
	assert.Equal(t, lifecycle.StateStarting, statuses[0].State, "the underlying lifecycle state must read as Starting while the Start is in flight")

	close(inner.release)
	require.NoError(t, <-startDone)

	statuses = r.AgentToolsetStatuses("root")
	assert.Equal(t, lifecycle.StateReady, statuses[0].State, "settled start reports Ready")
}

// TestAgentConfigInfo_Degrades verifies graceful degradation: an unknown agent
// yields the zero value, and a known agent on a team with no retained configs
// (e.g. the remote-style path) reports IsCurrent correctly and omits the
// config-only sections (skills, declared toolsets).
func TestAgentConfigInfo_Degrades(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "openai/gpt-5"}
	root := agent.New("root", "", agent.WithModel(prov))
	other := agent.New("other", "", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root, other)) // no retained configs

	r := &LocalRuntime{team: tm, agents: newAgentRouter(tm, "root")}

	assert.Equal(t, AgentConfigInfo{}, r.AgentConfigInfo(t.Context(), "missing"), "unknown agent -> zero value")

	got := r.AgentConfigInfo(t.Context(), "other")
	assert.False(t, got.IsCurrent, "non-current agent")
	assert.Nil(t, got.Skills, "no skills without retained config")
	assert.Nil(t, got.Options, "no enabled options")
	assert.Nil(t, got.Toolsets, "agent has no toolsets")
}
