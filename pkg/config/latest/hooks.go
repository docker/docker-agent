package latest

import (
	"fmt"
	"iter"
	"reflect"
	"strings"

	"github.com/docker/docker-agent/pkg/hooks/events"
)

// Events iterates over populated hook events using their persisted names.
func (h *HooksConfig) Events() iter.Seq2[string, HookMatcherConfigs] {
	return func(yield func(string, HookMatcherConfigs) bool) {
		if h == nil {
			return
		}
		v := reflect.ValueOf(h).Elem()
		for i := range v.NumField() {
			if v.Field(i).Len() == 0 {
				continue
			}
			name, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("json"), ",")
			var matchers HookMatcherConfigs
			switch hooks := v.Field(i).Interface().(type) {
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
		for i, matcher := range matchers {
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
