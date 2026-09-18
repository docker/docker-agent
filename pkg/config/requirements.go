package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
)

// Feature is an optional capability a config may rely on beyond model
// providers and toolsets. Embedders that load third-party configs can require
// each to be enabled explicitly (see teamloader.WithStrict).
type Feature string

const (
	// FeatureCodeMode covers agents setting code_mode_tools, which needs the
	// JavaScript tool wrapper (see teamloader.WithCodeMode).
	FeatureCodeMode Feature = "code_mode"
	// FeatureDeferredTools covers toolsets using `defer` (see
	// teamloader.WithDeferredTools).
	FeatureDeferredTools Feature = "deferred_tools"
	// FeatureExternalAgents covers sub_agents, handoffs and force_handoff
	// entries that reference another agent by OCI reference or URL.
	FeatureExternalAgents Feature = "external_agents"
	// FeatureHarness covers agents delegating to a coding harness CLI.
	FeatureHarness Feature = "harness"
	// FeatureHooks covers agent lifecycle hooks, which run host commands.
	FeatureHooks Feature = "hooks"
	// FeatureSkills covers agents loading skills from disk or inline.
	FeatureSkills Feature = "skills"
	// FeatureToon covers toolsets using `toon` output encoding (see
	// teamloader.WithToon).
	FeatureToon Feature = "toon"
)

// Requirements lists what a config needs from the host application to be
// loaded: model provider types, toolset types and optional features. Each
// entry maps to the config locations that need it, so a rejection can point
// at the offending lines.
type Requirements struct {
	// Providers maps a provider type, as the provider registry keys it (see
	// provider.ResolveType), to the locations using it.
	Providers map[string][]string
	// Toolsets maps a toolset type to the locations declaring it.
	Toolsets map[string][]string
	// Features maps an optional feature to the locations enabling it.
	Features map[Feature][]string
}

// Requires reports everything cfg relies on. It expects a config returned by
// Load: model references are already normalised into cfg.Models, and reusable
// toolset definitions are already merged onto each agent. Every declared
// model is included, not only those an agent uses, because the model picker
// can switch to any of them at runtime.
func Requires(cfg *latest.Config) Requirements {
	r := Requirements{
		Providers: map[string][]string{},
		Toolsets:  map[string][]string{},
		Features:  map[Feature][]string{},
	}

	for _, name := range slices.Sorted(maps.Keys(cfg.Models)) {
		r.model(cfg, cfg.Models[name], "models."+name)
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Toolsets)) {
		r.toolset(cfg, cfg.Toolsets[name], "toolsets."+name)
	}

	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		loc := "agents." + a.Name

		for i, ref := range a.GetFallbackModels() {
			r.modelRef(cfg, ref, fmt.Sprintf("%s.fallback.models[%d]", loc, i))
		}
		if ref := EffectiveCompactionModelRef(cfg, a); ref != "" {
			r.modelRef(cfg, ref, loc+".compaction_model")
		}
		for i, ts := range a.Toolsets {
			r.toolset(cfg, ts, fmt.Sprintf("%s.toolsets[%d]", loc, i))
		}
		for i, skill := range a.Skills.Inline {
			if skill.Model != "" {
				r.modelRef(cfg, skill.Model, fmt.Sprintf("%s.skills.inline[%d].model", loc, i))
			}
		}

		if a.CodeModeTools {
			r.feature(FeatureCodeMode, loc+".code_mode_tools")
		}
		if a.Harness != nil {
			r.feature(FeatureHarness, loc+".harness")
		}
		if !a.Hooks.IsEmpty() {
			r.feature(FeatureHooks, loc+".hooks")
		}
		if a.Skills.Enabled() {
			r.feature(FeatureSkills, loc+".skills")
		}
		r.agentRefs(a.SubAgents, loc+".sub_agents")
		r.agentRefs(a.Handoffs, loc+".handoffs")
		if a.ForceHandoff != "" {
			r.agentRefs([]string{a.ForceHandoff}, loc+".force_handoff")
		}
	}

	return r
}

// model records the provider a declared model resolves to, plus the models
// it delegates to for routing, titles and compaction.
func (r *Requirements) model(cfg *latest.Config, m latest.ModelConfig, loc string) {
	if m.IsFirstAvailable() {
		// Resolved into a concrete provider/model against the environment at
		// load time; nothing to record until then.
		return
	}
	r.provider(cfg, m, loc)
	for i, rule := range m.Routing {
		r.modelRef(cfg, rule.Model, fmt.Sprintf("%s.routing[%d].model", loc, i))
	}
	if m.TitleModel != "" {
		r.modelRef(cfg, m.TitleModel, loc+".title_model")
	}
	if m.CompactionModel != "" {
		r.modelRef(cfg, m.CompactionModel, loc+".compaction_model")
	}
}

// modelRef records the provider behind a model reference: a named model or
// an inline "provider/model" spec. Named models are already recorded by
// Requires, so only inline specs add anything.
func (r *Requirements) modelRef(cfg *latest.Config, ref, loc string) {
	if _, named := cfg.Models[ref]; named {
		return
	}
	inline, err := latest.ParseModelRef(ref)
	if err != nil {
		return
	}
	r.provider(cfg, inline, loc)
}

func (r *Requirements) provider(cfg *latest.Config, m latest.ModelConfig, loc string) {
	if m.Provider == "" {
		return
	}
	resolved := provider.ResolveType(&m, cfg.Providers)
	if resolved != m.Provider {
		loc = fmt.Sprintf("%s (provider %q)", loc, m.Provider)
	}
	r.Providers[resolved] = append(r.Providers[resolved], loc)
}

// toolset records a toolset's type, the model it routes tool results to, if
// any, and the optional features its declaration enables.
func (r *Requirements) toolset(cfg *latest.Config, ts latest.Toolset, loc string) {
	if ts.Type == "" {
		return
	}
	r.Toolsets[ts.Type] = append(r.Toolsets[ts.Type], loc)
	if ts.Model != "" {
		r.modelRef(cfg, ts.Model, loc+".model")
	}
	if ts.Toon != "" {
		r.feature(FeatureToon, loc+".toon")
	}
	if !ts.Defer.IsEmpty() {
		r.feature(FeatureDeferredTools, loc+".defer")
	}
}

func (r *Requirements) feature(f Feature, loc string) {
	r.Features[f] = append(r.Features[f], loc)
}

func (r *Requirements) agentRefs(refs []string, loc string) {
	for i, ref := range refs {
		if IsExternalReference(ref) {
			r.feature(FeatureExternalAgents, fmt.Sprintf("%s[%d]", loc, i))
		}
	}
}

// Check compares the requirements against what the application enabled and
// returns nil when everything is satisfied. Otherwise it returns an
// *UnsupportedError listing every unmet requirement at once.
func (r *Requirements) Check(hasProvider, hasToolset func(string) bool, features []Feature) error {
	var u UnsupportedError
	for _, p := range slices.Sorted(maps.Keys(r.Providers)) {
		if !hasProvider(p) {
			u.Providers = append(u.Providers, unsupported{p, r.Providers[p]})
		}
	}
	for _, t := range slices.Sorted(maps.Keys(r.Toolsets)) {
		if !hasToolset(t) {
			u.Toolsets = append(u.Toolsets, unsupported{t, r.Toolsets[t]})
		}
	}
	for _, f := range slices.Sorted(maps.Keys(r.Features)) {
		if !slices.Contains(features, f) {
			u.Features = append(u.Features, unsupported{string(f), r.Features[f]})
		}
	}
	if len(u.Providers)+len(u.Toolsets)+len(u.Features) == 0 {
		return nil
	}
	return &u
}

type unsupported struct {
	Name      string
	Locations []string
}

// UnsupportedError reports config items the application did not enable.
type UnsupportedError struct {
	Providers []unsupported
	Toolsets  []unsupported
	Features  []unsupported
}

func (e *UnsupportedError) Error() string {
	var b strings.Builder
	b.WriteString("config uses items this application does not enable:")
	for _, p := range e.Providers {
		fmt.Fprintf(&b, "\n  provider %q at %s", p.Name, strings.Join(p.Locations, ", "))
	}
	for _, t := range e.Toolsets {
		fmt.Fprintf(&b, "\n  toolset type %q at %s", t.Name, strings.Join(t.Locations, ", "))
	}
	for _, f := range e.Features {
		fmt.Fprintf(&b, "\n  feature %q at %s", f.Name, strings.Join(f.Locations, ", "))
	}
	return b.String()
}
