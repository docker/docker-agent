package latest

import (
	"fmt"
	"iter"
	"reflect"
	"strings"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/hooks/events"
)

// Clone returns a deep copy of the hooks configuration.
func (h *HooksConfig) Clone() *HooksConfig {
	if h == nil {
		return nil
	}
	var cloned HooksConfig
	types.CloneThroughJSON(h, &cloned)
	return &cloned
}

// Events iterates over populated hook events using their persisted names.
func (h *HooksConfig) Events() iter.Seq2[string, HookMatcherConfigs] {
	return func(yield func(string, HookMatcherConfigs) bool) {
		if h == nil {
			return
		}
		v := reflect.ValueOf(h).Elem()
		for metadata, field := range v.Fields() {
			if field.Len() == 0 {
				continue
			}
			name, _, _ := strings.Cut(metadata.Tag.Get("json"), ",")
			var matchers HookMatcherConfigs
			switch hooks := field.Interface().(type) {
			case HookDefinitions:
				matchers = HookMatcherConfigs{{Hooks: hooks}}
			case HookMatcherConfigs:
				matchers = hooks
			}
			if !yield(name, matchers) {
				return
			}
		}
	}
}

// IsEmpty reports whether no hook events are configured.
func (h *HooksConfig) IsEmpty() bool {
	for range h.Events() {
		return false
	}
	return true
}

// Validate checks hook definitions and event-specific options.
func (h *HooksConfig) Validate() error {
	for event, matchers := range h.Events() {
		contract, ok := events.Lookup(event)
		if !ok {
			return fmt.Errorf("hooks.%s: unknown event", event)
		}
		if !contract.CanBlock {
			for _, matcher := range matchers {
				for _, hook := range matcher.Hooks {
					if hook.OnError == "block" {
						return fmt.Errorf("hooks.%s: on_error block is not supported by this event", event)
					}
				}
			}
		}
		for _, matcher := range matchers {
			for _, hook := range matcher.Hooks {
				if hook.Type != "model" {
					continue
				}
				if hook.Schema == "guard_decision" && !contract.CanBlock {
					return fmt.Errorf("hooks.%s: guard_decision requires a blocking event", event)
				}
				if hook.Schema == "pre_tool_use_decision" && !contract.Permission() {
					return fmt.Errorf("hooks.%s: pre_tool_use_decision requires an approval event", event)
				}
				if (event == "skill_content_guard" || event == "prompt_file_guard") && hook.Schema != "guard_decision" {
					return fmt.Errorf("hooks.%s: model hooks require schema guard_decision", event)
				}
			}
		}
		for i, matcher := range matchers {
			for _, hook := range matcher.Hooks {
				if err := validateEvaluatorPlacement(event, contract, hook); err != nil {
					return err
				}
				if contract.Control && hook.Type == "model" {
					return fmt.Errorf("hooks.%s: model hooks cannot select routes; use a command or choice evaluator", event)
				}
				if hook.RoutingPolicy != nil && !contract.Control {
					return fmt.Errorf("hooks.%s: routing_policy is only supported on before_agent_run and after_agent_complete", event)
				}
			}
			if contract.ToolMatched {
				if err := matcher.validate(event, i); err != nil {
					return err
				}
			} else {
				for j, hook := range matcher.Hooks {
					if err := hook.validate(event, j); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func validateEvaluatorPlacement(event string, contract events.Contract, hook HookDefinition) error {
	if hook.Type != "evaluator" {
		return nil
	}
	switch {
	case contract.Control:
		if hook.EvaluatorPolicy != nil {
			return fmt.Errorf("hooks.%s: evaluator hooks on this event use routing_policy, not evaluator_policy", event)
		}
	case event == "tool_guard":
		if hook.RoutingPolicy != nil {
			return fmt.Errorf("hooks.%s: evaluator hooks on tool_guard use evaluator_policy, not routing_policy", event)
		}
	default:
		return fmt.Errorf("hooks.%s: evaluator hooks are only supported on tool_guard, before_agent_run, and after_agent_complete", event)
	}
	return nil
}
