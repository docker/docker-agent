package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
