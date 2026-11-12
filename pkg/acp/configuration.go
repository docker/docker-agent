package acp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/session"
)

type sessionConfiguration struct {
	Options []acp.SessionConfigOption
	Modes   *acp.SessionModeState
}

type modelSelection struct {
	value    string
	thinking string
	snapshot agent.ModelOverrideSnapshot
}

func (s *Session) rememberModelSelection(name, value, thinking string, snapshot agent.ModelOverrideSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modelSelections == nil {
		s.modelSelections = make(map[string]modelSelection)
	}
	s.modelSelections[name] = modelSelection{value: value, thinking: thinking, snapshot: snapshot}
}

type configurationRuntime interface {
	HasActiveWork() bool
	AgentThinkingConfiguration(agentName string) ([]effort.Level, effort.Level)
}

func safetyModes() []acp.SessionMode {
	return []acp.SessionMode{
		{Id: "default", Name: "Default", Description: new("Read-only annotated tools auto-approve; other calls ask.")},
		{Id: "strict", Name: "Strict", Description: new("Ask for every tool call unless an explicit rule permits it.")},
		{Id: "balanced", Name: "Balanced", Description: new("Auto-approve classifier-safe calls; ask otherwise.")},
		{Id: "restricted", Name: "Restricted", Description: new("Auto-approve classifier-safe calls; deny otherwise unless rules override.")},
		{Id: "autonomous", Name: "Autonomous", Description: new("Auto-approve tools except deny/session-ask rules, tool guards, and preempt_yolo hooks.")},
	}
}

func selectOption(id, name string, category acp.SessionConfigOptionCategory, current string, values []acp.SessionConfigSelectOption) acp.SessionConfigOption {
	options := acp.SessionConfigSelectOptionsUngrouped(values)
	return acp.SessionConfigOption{Select: &acp.SessionConfigOptionSelect{Id: acp.SessionConfigId(id), Name: name, Type: "select", Category: &category, CurrentValue: acp.SessionConfigValueId(current), Options: acp.SessionConfigSelectOptions{Ungrouped: &options}}}
}

// configuration reads local state only: no model catalog, provider creation, or tool discovery.
func (s *Session) configuration(ctx context.Context) sessionConfiguration {
	if !s.configEnabled {
		return sessionConfiguration{}
	}
	modes := safetyModes()
	current := string(s.sess.GetSafetyPolicy())
	if current == "" {
		current = "default"
	}
	state := sessionConfiguration{Modes: &acp.SessionModeState{AvailableModes: modes, CurrentModeId: acp.SessionModeId(current)}}
	var values []acp.SessionConfigSelectOption
	for _, mode := range modes {
		values = append(values, acp.SessionConfigSelectOption{Value: acp.SessionConfigValueId(mode.Id), Name: mode.Name, Description: mode.Description})
	}
	state.Options = append(state.Options, selectOption("mode", "Docker Agent tool safety", acp.SessionConfigOptionCategoryMode, current, values))
	if s.team == nil || !s.rt.SupportsModelSwitching() {
		return state
	}
	if rt, ok := s.rt.(configurationRuntime); ok && rt.HasActiveWork() {
		return state
	}
	name := s.rt.CurrentAgentName(ctx)
	selected, err := s.team.Agent(name)
	if err != nil || selected.HasHarness() {
		return state
	}
	models := selected.EffectiveModels()
	modelValue := "default"
	if selected.HasModelOverride() {
		modelValue = "current"
		if len(models) == 1 {
			cfg := models[0].BaseConfig().ModelConfig
			if _, ok := s.configModels[cfg.Name]; ok && cfg.Name != "" {
				modelValue = "model:" + cfg.Name
			}
		}
	}
	s.mu.Lock()
	selection, tracked := s.modelSelections[name]
	s.mu.Unlock()
	if tracked && selection.snapshot == selected.SnapshotModelOverride() {
		modelValue = selection.value
	}
	modelChoices := []acp.SessionConfigSelectOption{{Value: "default", Name: "Agent default"}}
	for _, key := range slices.Sorted(maps.Keys(s.configModels)) {
		if selectableConfiguredModel(s.configModels[key], s.configModels) {
			modelChoices = append(modelChoices, acp.SessionConfigSelectOption{Value: acp.SessionConfigValueId("model:" + key), Name: key})
		}
	}
	if !slices.ContainsFunc(modelChoices, func(choice acp.SessionConfigSelectOption) bool { return string(choice.Value) == modelValue }) {
		modelValue = "current"
	}
	if modelValue == "current" {
		modelChoices = append(modelChoices, acp.SessionConfigSelectOption{Value: "current", Name: "Current runtime model (not a configured choice)"})
	}
	state.Options = append(state.Options, selectOption("model", "Model · "+name, acp.SessionConfigOptionCategoryModel, modelValue, modelChoices))
	if rt, ok := s.rt.(configurationRuntime); ok && modelValue != "current" {
		levels, level := rt.AgentThinkingConfiguration(name)
		if len(levels) > 0 {
			choices := []acp.SessionConfigSelectOption{{Value: "default", Name: "Configured/default budget"}}
			for _, l := range levels {
				choices = append(choices, acp.SessionConfigSelectOption{Value: acp.SessionConfigValueId(l), Name: string(l)})
			}
			current := "default"
			if tracked && selection.snapshot == selected.SnapshotModelOverride() && selection.thinking != "" {
				level = effort.Level(selection.thinking)
			}
			if level != "" && slices.Contains(levels, level) {
				current = string(level)
			}
			state.Options = append(state.Options, selectOption("thought_level", "Reasoning · "+name, acp.SessionConfigOptionCategoryThoughtLevel, current, choices))
		}
	}
	return state
}

func (a *Agent) refreshConfiguration(ctx context.Context, s *Session) {
	if !s.configEnabled || a.conn == nil {
		return
	}
	if rt, ok := s.rt.(configurationRuntime); ok && rt.HasActiveWork() {
		return
	}
	if err := a.emitConfiguration(ctx, s, s.configuration(ctx)); err != nil {
		slog.DebugContext(ctx, "Failed to emit session configuration", "error", err)
	}
}

func (a *Agent) emitConfiguration(ctx context.Context, s *Session, state sessionConfiguration) error {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	if reflect.DeepEqual(s.lastConfig, &state) {
		return nil
	}
	if a.conn != nil {
		if err := a.sendUpdate(ctx, s.id, acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{ConfigOptions: state.Options}}); err != nil {
			return err
		}
		if s.lastConfig == nil || s.lastConfig.Modes.CurrentModeId != state.Modes.CurrentModeId {
			if err := a.sendUpdate(ctx, s.id, acp.SessionUpdate{CurrentModeUpdate: &acp.SessionCurrentModeUpdate{CurrentModeId: state.Modes.CurrentModeId}}); err != nil {
				return err
			}
		}
	}
	s.lastConfig = &state
	return nil
}

// SetSessionConfigOption accepts stable select values only.
func (a *Agent) SetSessionConfigOption(ctx context.Context, params acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	var meta map[string]any
	if params.ValueId != nil {
		meta = params.ValueId.Meta
	} else if params.Boolean != nil {
		meta = params.Boolean.Meta
	}
	ctx, span := startACPRequest(ctx, "session/set_config_option", meta)
	defer span.End()
	if params.ValueId == nil || params.Boolean != nil {
		return acp.SetSessionConfigOptionResponse{}, acp.NewInvalidParams("a select value is required")
	}
	p := params.ValueId
	state, err := a.setConfiguration(ctx, string(p.SessionId), string(p.ConfigId), string(p.Value))
	return acp.SetSessionConfigOptionResponse{ConfigOptions: state.Options}, err
}

func (a *Agent) SetSessionMode(ctx context.Context, params acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	ctx, span := startACPRequest(ctx, "session/set_mode", params.Meta)
	defer span.End()
	_, err := a.setConfiguration(ctx, string(params.SessionId), "mode", string(params.ModeId))
	return acp.SetSessionModeResponse{}, err
}

func (a *Agent) setConfiguration(ctx context.Context, sid, id, value string) (sessionConfiguration, error) {
	empty := sessionConfiguration{}
	if sid == "" || id == "" || value == "" {
		return empty, acp.NewInvalidParams("sessionId, configId and value are required")
	}
	ctx, op, err := a.beginSessionConstruction(ctx, sid)
	if err != nil {
		return empty, err
	}
	defer a.finishOperation(op)
	a.mu.Lock()
	s := a.sessions[sid]
	a.mu.Unlock()
	if s == nil {
		return empty, sessionNotFound(sid)
	}
	if err := a.reserveReconnect(ctx, s, true); err != nil {
		return empty, err
	}
	defer s.finishLoading()
	rt, ok := s.rt.(configurationRuntime)
	if !ok || rt.HasActiveWork() {
		return empty, acp.NewInvalidRequest("session has active work; retry configuration after it finishes")
	}
	state := s.configuration(ctx)
	var option *acp.SessionConfigOptionSelect
	for _, candidate := range state.Options {
		if candidate.Select != nil && string(candidate.Select.Id) == id {
			option = candidate.Select
			break
		}
	}
	if option == nil || option.Options.Ungrouped == nil || !slices.ContainsFunc(*option.Options.Ungrouped, func(choice acp.SessionConfigSelectOption) bool { return string(choice.Value) == value }) {
		return empty, acp.NewInvalidParams("unknown configuration option or value")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if string(option.CurrentValue) != value || value == "default" || id == "model" {
		switch id {
		case "mode":
			policy := session.SafetyPolicy(value)
			if value == "default" {
				policy = ""
			}
			staged := s.sess.Clone()
			staged.Origin = s.sess.Origin
			staged.SetSafetyPolicy(policy)
			if err := a.sessionStore.UpdateSession(ctx, staged); err != nil {
				return empty, fmt.Errorf("persisting safety mode: %w", err)
			}
			s.sess.SetSafetyPolicy(policy)
		case "model", "thought_level":
			if id == "model" && value == "current" {
				break
			}
			name := s.rt.CurrentAgentName(ctx)
			selected, err := s.team.Agent(name)
			if err != nil {
				return empty, err
			}
			previous := selected.SnapshotModelOverride()
			ref := ""
			modelChanged := false
			switch {
			case id == "model":
				if value == "current" {
					break
				}
				ref = strings.TrimPrefix(value, "model:")
				if value == "default" {
					ref = ""
				}
				if string(option.CurrentValue) != value || value == "default" {
					err = s.rt.SetAgentModel(ctx, name, ref)
					modelChanged = true
				}
			case value == "default":
				for _, o := range state.Options {
					if o.Select != nil && o.Select.Id == "model" && strings.HasPrefix(string(o.Select.CurrentValue), "model:") {
						ref = strings.TrimPrefix(string(o.Select.CurrentValue), "model:")
					}
				}
				err = s.rt.SetAgentModel(ctx, name, ref)
			default:
				_, err = s.rt.SetAgentThinkingLevel(ctx, name, effort.Level(value))
			}
			if err != nil {
				if ref != "" {
					if authErr := a.modelAuthenticationError(ctx, s, ref); authErr != nil {
						return empty, authErr
					}
				}
				return empty, acp.NewInvalidParams(err.Error())
			}
			applied := selected.SnapshotModelOverride()
			rollback := func() { selected.RestoreModelOverride(previous, applied) }
			if err := ctx.Err(); err != nil {
				rollback()
				return empty, err
			}
			if id == "model" {
				staged := s.sess.Clone()
				staged.Origin = s.sess.Origin
				staged.SetAgentModelOverride(name, ref)
				if err := a.sessionStore.UpdateSession(ctx, staged); err != nil {
					rollback()
					return empty, fmt.Errorf("persisting model selection: %w", err)
				}
				s.sess.SetAgentModelOverride(name, ref)
			}
			selectedValue := value
			if id == "thought_level" {
				for _, o := range state.Options {
					if o.Select != nil && o.Select.Id == "model" {
						selectedValue = string(o.Select.CurrentValue)
					}
				}
			}
			thinking := ""
			if id == "thought_level" && value != "default" {
				thinking = value
			}
			if id == "model" && !modelChanged {
				s.mu.Lock()
				if previous, ok := s.modelSelections[name]; ok && previous.snapshot == applied {
					thinking = previous.thinking
				}
				s.mu.Unlock()
			}
			s.rememberModelSelection(name, selectedValue, thinking, applied)
			s.mu.Lock()
			s.contextLimit = 0
			if s.rootUsage != nil {
				s.rootUsage.ContextLimit = 0
			}
			s.mu.Unlock()
		default:
			return empty, errors.New("unsupported configuration")
		}
	}
	state = s.configuration(ctx)
	if err := a.emitConfiguration(ctx, s, state); err != nil {
		return state, err
	}
	return state, nil
}

// Runtime model switching supports a flat alloy, not nested alloy references.
func selectableConfiguredModel(cfg latest.ModelConfig, models map[string]latest.ModelConfig) bool {
	if cfg.Provider != "" || !strings.Contains(cfg.Model, ",") {
		return true
	}
	for ref := range strings.SplitSeq(cfg.Model, ",") {
		if nested, ok := models[strings.TrimSpace(ref)]; ok && nested.Provider == "" && strings.Contains(nested.Model, ",") {
			return false
		}
	}
	return true
}
