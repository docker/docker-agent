// Package inspection provides side-effect-free views of agents and toolsets.
package inspection

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

// Service reads a team without starting toolsets or execution drivers.
type Service struct {
	Team         *team.Team
	CurrentAgent string
}

// ToolsetState is the coarse lifecycle bucket the agent inspector renders as a
// status glyph. It collapses the full lifecycle.State machine into the three
// distinctions a reader cares about: serving (started), not running (stopped),
// or broken (error).
type ToolsetState string

const (
	ToolsetStarted ToolsetState = "started"
	ToolsetStopped ToolsetState = "stopped"
	ToolsetError   ToolsetState = "error"
)

// ToolsetDetail describes one configured toolset for the agent inspector: its
// display name, kind, lifecycle bucket and the tools it exposes. Tools holds
// the live tool names when the toolset is started, otherwise the declared
// `tools:` allow-list from the retained config, and is empty when neither is
// available (a not-yet-started toolset with no explicit allow-list).
type ToolsetDetail struct {
	Name  string
	Kind  string
	State ToolsetState
	Tools []string
}

// AgentConfigInfo is the static-plus-live dataset behind the read-only agent
// inspector modal. The static parts (sub-agents, handoffs, fallbacks, skills,
// limits, option flags, declared toolset allow-lists) are derived from the
// resolved *agent.Agent and the retained config without starting any toolset;
// the live parts (per-toolset lifecycle state, started tool names, IsCurrent)
// reflect the running team. Remote runtimes (which hold no local team) return
// the zero value, so the modal omits every config-derived section.
type AgentConfigInfo struct {
	SubAgents []string // sub-agent names, sorted
	Handoffs  []string // handoff target names, sorted
	Fallbacks []string // fallback model ids ("provider/model"), in priority order
	Skills    []string // configured skill names (inline + included + used), sorted

	MaxIterations           int // 0 when unset
	NumHistoryItems         int // 0 when unset
	MaxConsecutiveToolCalls int // 0 when unset

	Options  []string        // enabled option flags (add-date, redact-secrets, ...)
	Toolsets []ToolsetDetail // per-toolset live state + tools, in declaration order

	IsCurrent bool // true when this is the live current agent

	ExecutionCapabilities *ExecutionCapabilities
}

// AgentConfigInfo returns the named agent's inspector dataset. It inspects the
// resolved agent and the retained config, reading live tool names only from
// already-started toolsets (never starting one), so it is safe to call for any
// agent whether or not it has run. Unknown agents yield the zero value, so the
// modal omits the corresponding sections.
func (r Service) AgentConfigInfo(ctx context.Context, agentName string) AgentConfigInfo {
	a, err := r.Team.Agent(agentName)
	if err != nil || a == nil {
		return AgentConfigInfo{}
	}

	cfg, hasCfg := r.Team.AgentConfig(agentName)

	var subAgents []string
	for _, sub := range a.SubAgents() {
		if sub != nil {
			subAgents = append(subAgents, sub.Name())
		}
	}

	var handoffs []string
	for _, h := range a.Handoffs() {
		if h != nil {
			handoffs = append(handoffs, h.Name())
		}
	}

	var fallbacks []string
	for _, p := range a.FallbackModels() {
		if p != nil {
			fallbacks = append(fallbacks, p.ID().String())
		}
	}

	info := AgentConfigInfo{
		SubAgents:               UniqueNames(subAgents, true),
		Handoffs:                UniqueNames(handoffs, true),
		Fallbacks:               UniqueNames(fallbacks, false),
		MaxIterations:           a.MaxIterations(),
		NumHistoryItems:         a.NumHistoryItems(),
		MaxConsecutiveToolCalls: a.MaxConsecutiveToolCalls(),
		Options:                 agentOptionFlags(a, cfg, hasCfg),
		Toolsets:                toolsetDetails(ctx, a, cfg, hasCfg),
		IsCurrent:               r.CurrentAgent == agentName,
		ExecutionCapabilities:   Capabilities(a),
	}
	if hasCfg {
		info.Skills = configSkillNames(cfg)
	}
	return info
}

// agentOptionFlags lists the agent's enabled boolean options as stable,
// hyphenated display names. AddDate/AddEnvironmentInfo/RedactSecrets read the
// agent's effective (resolved) values; CodeModeTools is only knowable from the
// retained config because the runtime folds it into a single wrapper toolset.
func agentOptionFlags(a *agent.Agent, cfg latest.AgentConfig, hasCfg bool) []string {
	var opts []string
	if a.AddDate() {
		opts = append(opts, "add-date")
	}
	if a.AddEnvironmentInfo() {
		opts = append(opts, "add-environment-info")
	}
	if a.RedactSecrets() {
		opts = append(opts, "redact-secrets")
	}
	if hasCfg && cfg.CodeModeTools {
		opts = append(opts, "code-mode-tools")
	}
	return opts
}

// configSkillNames lists the cleanly-resolvable skill names from the agent
// config: inline skill names, the Include allow-list, and referenced skill
// groups (UseSkills). Skills auto-discovered from Sources (e.g. a local
// directory) are not enumerated here because doing so would require loading
// them from disk; the inspector notes this rather than starting that work.
func configSkillNames(cfg latest.AgentConfig) []string {
	var names []string
	for _, s := range cfg.Skills.Inline {
		names = append(names, s.Name)
	}
	names = append(names, cfg.Skills.Include...)
	names = append(names, cfg.UseSkills...)
	return UniqueNames(names, true)
}

// toolsetDetails builds one ToolsetDetail per live toolset of the agent,
// combining the side-effect-free lifecycle status with tool names: live names
// for started toolsets, otherwise the declared `tools:` allow-list keyed by the
// same name the registry assigns (cmp.Or(name, type)).
func toolsetDetails(ctx context.Context, a *agent.Agent, cfg latest.AgentConfig, hasCfg bool) []ToolsetDetail {
	toolSets := a.ToolSets()
	if len(toolSets) == 0 {
		return nil
	}
	var declared map[string][]string
	if hasCfg {
		declared = declaredToolNames(cfg)
	}
	infos := make([]ToolsetDetail, 0, len(toolSets))
	for _, ts := range toolSets {
		status := ToolsetStatus(ts)
		info := ToolsetDetail{
			Name:  status.Name,
			Kind:  status.Kind,
			State: toolsetStateBucket(status.State),
		}
		if live, ok := startedToolNames(ctx, ts); ok {
			info.Tools = live
		} else if names, ok := declared[status.Name]; ok {
			info.Tools = names
		}
		infos = append(infos, info)
	}
	return infos
}

// toolsetStateBucket collapses the lifecycle state machine into the inspector's
// three buckets. Failed is the only error state; Stopped (including a
// not-yet-started toolset) is stopped; everything else (Ready, Degraded,
// Starting, Restarting) reads as started/serving.
func toolsetStateBucket(s lifecycle.State) ToolsetState {
	switch s {
	case lifecycle.StateFailed:
		return ToolsetError
	case lifecycle.StateStopped:
		return ToolsetStopped
	default:
		return ToolsetStarted
	}
}

// startedToolNames returns the live tool names of ts, but only when it is a
// started toolset; the boolean is false for a not-yet-started toolset, or one
// whose Start is still in flight, so the caller can fall back to the declared
// allow-list without blocking on a long-running Start (e.g. RAG indexing).
// context.TODO is safe here: the toolset is already started, so listing
// returns its cached tools without a cancellable round-trip.
func startedToolNames(ctx context.Context, ts tools.ToolSet) ([]string, bool) {
	s, ok := tools.As[*tools.StartableToolSet](ts)
	if !ok {
		return nil, false
	}
	if started, _ := s.TryState(); !started {
		return nil, false
	}
	tl, err := s.Tools(ctx)
	if err != nil {
		return nil, false
	}
	names := make([]string, 0, len(tl))
	for i := range tl {
		if tl[i].Name != "" {
			names = append(names, tl[i].Name)
		}
	}
	return names, true
}

// declaredToolNames maps each configured toolset's display name to its declared
// `tools:` allow-list. The key matches the registry's naming
// (cmp.Or(name, type)) so it lines up with the live toolset's Name. Toolsets
// with no explicit allow-list (serve all tools) are omitted.
func declaredToolNames(cfg latest.AgentConfig) map[string][]string {
	m := make(map[string][]string, len(cfg.Toolsets))
	for i := range cfg.Toolsets {
		t := cfg.Toolsets[i]
		key := cmp.Or(t.Name, t.Type)
		if key == "" || len(t.Tools) == 0 {
			continue
		}
		m[key] = slices.Clone(t.Tools)
	}
	return m
}

// UniqueNames drops empty entries and de-duplicates names. By default it keeps
// the first-seen order (meaningful for declaration/priority order); when sorted
// is true it orders the result case-insensitively instead.
func UniqueNames(names []string, sorted bool) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if sorted {
		slices.SortFunc(out, func(a, b string) int {
			return strings.Compare(strings.ToLower(a), strings.ToLower(b))
		})
	}
	return out
}

// ToolsetStatus builds a ToolsetStatus for ts. tools.As walks the
// wrapper chain so Statable/Describer can live anywhere in the stack.
func ToolsetStatus(ts tools.ToolSet) tools.ToolsetStatus {
	status := tools.ToolsetStatus{
		Description: tools.DescribeToolSet(ts),
	}
	if kinder, ok := tools.As[tools.Kinder](ts); ok {
		status.Kind = kinder.Kind()
	}
	if statable, ok := tools.As[tools.Statable](ts); ok {
		info := statable.State()
		status.State = info.State
		status.LastError = info.LastError
		status.RestartCount = info.RestartCount
	} else {
		// Toolsets without a supervisor are considered ready by default;
		// the StartableToolSet wrapper would have surfaced an error
		// earlier if Start failed.
		status.State = lifecycleStateForUnsupervised(ts)
	}
	if startable, ok := ts.(*tools.StartableToolSet); ok {
		status.Restartable = startable.CanRestart()
	} else {
		_, status.Restartable = tools.As[tools.Restartable](ts)
	}
	status.Name = ToolsetName(ts, status.Description)
	return status
}

// lifecycleStateForUnsupervised reports the lifecycle state of a toolset with
// no Statable supervisor, using the wrapper's own non-blocking status probe
// so a toolset whose Start is legitimately still running (e.g. RAG indexing
// a large knowledge base) never stalls a status/introspection caller.
func lifecycleStateForUnsupervised(ts tools.ToolSet) lifecycle.State {
	s, ok := ts.(*tools.StartableToolSet)
	if !ok {
		return lifecycle.StateReady
	}
	started, inFlight := s.TryState()
	switch {
	case inFlight:
		return lifecycle.StateStarting
	case !started:
		return lifecycle.StateStopped
	default:
		return lifecycle.StateReady
	}
}

// ToolsetName picks a stable, user-visible name for a toolset. We look for
// any inner toolset that implements tools.Named (walked via tools.As so
// wrappers like StartableToolSet are transparent). The registry adds a
// WithName wrapper for every built-in toolset so this is reachable for
// almost every toolset; fallback uses the description ("mcp(stdio cmd=...)"),
// which is still better than the Go type name.
func ToolsetName(ts tools.ToolSet, fallback string) string {
	if name := tools.GetName(ts); name != "" {
		return name
	}
	return fallback
}
