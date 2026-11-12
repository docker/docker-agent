package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/hooks/events"
)

func parseStdoutJSON(stdout string, strict bool) (*Output, error) {
	s := strings.TrimSpace(stdout)
	if s == "" {
		return nil, nil
	}
	if !strings.HasPrefix(s, "{") {
		if strict {
			return nil, errors.New("strict_output requires a JSON object or empty stdout")
		}
		return nil, nil
	}
	var parsed Output
	decoder := json.NewDecoder(strings.NewReader(s))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("invalid hook output: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("hook output must contain a single JSON object")
	}
	return &parsed, nil
}

// validateOutput checks verdicts for every hook and capabilities for strict hooks.
func validateOutput(event EventType, out *Output, strict bool) error {
	c := EventContract(event)
	if strict && out.SuppressOutput {
		return errors.New("suppress_output is not supported; use stderr for diagnostics")
	}
	if out.Decision != "" && out.Decision != DecisionBlockValue {
		return fmt.Errorf("invalid decision %q: expected block", out.Decision)
	}
	if strict && !c.CanBlock && (!out.ShouldContinue() || out.IsBlocked()) {
		return fmt.Errorf("%s does not support blocking output", c.Name)
	}
	hso := out.HookSpecificOutput
	if hso == nil {
		return nil
	}
	if strict && hso.HookEventName != "" && string(hso.HookEventName) != c.Name {
		return fmt.Errorf("output hook_event_name %q does not match %s", hso.HookEventName, c.Name)
	}
	switch hso.PermissionDecision {
	case "", DecisionAllow, DecisionAsk, DecisionDeny:
	default:
		return fmt.Errorf("invalid permission_decision %q: expected allow, ask, or deny", hso.PermissionDecision)
	}
	if err := validateTransition(c, hso.Transition); err != nil {
		return err
	}
	if !strict {
		return nil
	}
	for _, field := range []struct {
		name      string
		present   bool
		supported bool
	}{
		{"permission_decision", hso.PermissionDecision != "" || hso.PermissionDecisionReason != "", c.Permission()},
		{"updated_input", hso.UpdatedInput != nil, c.Rewrite == events.RewriteToolInput},
		{"updated_messages", hso.UpdatedMessages != nil, c.Rewrite == events.RewriteMessages},
		{"updated_tool_response", hso.UpdatedToolResponse != nil, c.Rewrite == events.RewriteToolResponse},
		{"metadata", hso.Metadata != nil, c.Metadata},
		{"additional_context", hso.AdditionalContext != "", c.Context},
		{"instruction_context", hso.InstructionContext != nil, c.Instructions},
		{"summary", hso.Summary != "", c.Summary},
	} {
		if field.present && !field.supported {
			return fmt.Errorf("%s does not support %s", c.Name, field.name)
		}
	}
	return nil
}

// validateTransition rejects transitions on non-control events regardless of
// strictness and malformed ones on control events. Targets are authorized by
// the runtime, which owns the agent's routing allowlist.
func validateTransition(c events.Contract, t *Transition) error {
	if t == nil {
		return nil
	}
	if !c.Control {
		return fmt.Errorf("%s does not support transition", c.Name)
	}
	if t.Action != TransitionActionRoute {
		return fmt.Errorf("invalid transition action %q: expected %s", t.Action, TransitionActionRoute)
	}
	if strings.TrimSpace(t.Agent) == "" {
		return errors.New("route transition requires an agent")
	}
	return nil
}

// parseContentGuardOutput rejects ambiguous JSON before ordinary protocol decoding.
func parseContentGuardOutput(stdout string) (*Output, error) {
	fields, err := contentGuardFields(stdout, "continue", "stop_reason", "decision", "reason", "system_message", "suppress_output", "hook_specific_output")
	if err != nil {
		return nil, err
	}
	if nested, ok := fields["hook_specific_output"]; ok && string(nested) != "null" {
		if _, err := contentGuardFields(string(nested), "hook_event_name"); err != nil {
			return nil, err
		}
	}
	return parseStdoutJSON(stdout, true)
}

func contentGuardFields(raw string, allowed ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("content guard requires a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || !slices.Contains(allowed, key) {
			return nil, errors.New("unknown content guard field")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate content guard field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.New("invalid content guard field")
		}
		fields[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("invalid content guard output")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("content guard requires a single JSON object")
	}
	return fields, nil
}
