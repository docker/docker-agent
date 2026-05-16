package main

import (
	"strconv"
	"strings"

	"github.com/dgageot/rubocop-go/cop"
)

// HookConfigSync enforces that the EventXxx constants in pkg/hooks/types.go
// stay in lock-step with the HooksConfig fields in pkg/config/latest/types.go.
//
// The two are coupled: every hook event the runtime knows how to dispatch
// has to be configurable in the agent YAML, and every YAML field has to
// correspond to an event the runtime actually fires. The mapping is by
// snake-case wire string:
//
//	pkg/hooks/types.go        : EventPreToolUse EventType = "pre_tool_use"
//	pkg/config/latest/types.go: PreToolUse []HookMatcherConfig `json:"pre_tool_use,…"`
//
// Drift in either direction is silently broken at runtime:
//
//   - A new EventXxx constant without a matching HooksConfig field means
//     the YAML schema cannot express that hook — users have no way to
//     register one, and the event fires with no listeners.
//   - A new HooksConfig field without a matching EventXxx constant means
//     the runtime parses the YAML but never dispatches the event — the
//     hook is wired up but inert.
//
// Neither failure mode produces a build error or a runtime warning. The
// cop runs on pkg/config/latest/types.go (where the diagnostic anchors
// on the HooksConfig type spec) and parses pkg/hooks/types.go from disk
// for the source of truth.
var HookConfigSync = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/HookConfigSync",
		Description: "EventXxx constants in pkg/hooks/types.go must match HooksConfig fields in pkg/config/latest",
		Severity:    cop.Error,
	},
	Scope: cop.OnlyFile("pkg/config/latest/types.go"),
	Run: func(p *cop.Pass) {
		// pkg/config/latest/types.go ↔ ../../hooks/types.go
		hookFile, err := p.ParseSibling("../../hooks/types.go")
		if err != nil {
			return
		}
		hookEvents := cop.StringConstsIn(hookFile, func(name string) bool {
			return strings.HasPrefix(name, "Event")
		})
		if len(hookEvents) == 0 {
			return
		}

		ts, st := p.StructType("HooksConfig")
		if ts == nil {
			// Schema didn't ship HooksConfig (or this isn't the right
			// types.go) — nothing meaningful the cop can say.
			return
		}

		// Build Go-name -> wire-name and the reverse, to diff in both directions.
		cfgFields := map[string]string{}
		for _, fld := range st.Fields.List {
			opts, ok := cop.ParseTagOptions(cop.FieldTag(fld), "json")
			if !ok || !opts.HasName() {
				continue
			}
			for _, n := range fld.Names {
				cfgFields[n.Name] = opts.Name
			}
		}
		cfgByJSON := map[string]string{}
		for goName, jsonName := range cfgFields {
			cfgByJSON[jsonName] = goName
		}

		// Direction 1: every event must have a config field.
		var missingFields []string
		for constName, wire := range hookEvents {
			if _, ok := cfgByJSON[wire]; !ok {
				missingFields = append(missingFields, constName+"="+strconv.Quote(wire))
			}
		}
		p.ReportMissing(ts.Name,
			"HooksConfig is missing field(s) for hook event(s): %s", missingFields)

		// Direction 2: every config field must have an event constant.
		wireSet := map[string]string{}
		for n, w := range hookEvents {
			wireSet[w] = n
		}
		var orphanFields []string
		for goName, jsonName := range cfgFields {
			if _, ok := wireSet[jsonName]; !ok {
				orphanFields = append(orphanFields, goName+" json:"+strconv.Quote(jsonName))
			}
		}
		p.ReportMissing(ts.Name,
			"HooksConfig field(s) without a matching EventXxx constant in pkg/hooks/types.go: %s",
			orphanFields)
	},
}
