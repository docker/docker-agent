package hooks

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// ShapeGuardDecision is an explicit allow/deny verdict for blocking events.
const ShapeGuardDecision = "guard_decision"

var _ = registerGuardDecision()

func registerGuardDecision() bool {
	if err := errors.Join(
		RegisterResponseShape(ShapeGuardDecision, guardDecisionShape),
		RegisterResponseSchema(ShapeGuardDecision, &latest.StructuredOutput{
			Name:        ShapeGuardDecision,
			Description: "Allow or deny the operation",
			Strict:      true,
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"decision": map[string]any{"type": "string", "enum": []string{"allow", "deny"}},
					"reason":   map[string]any{"type": "string"},
				},
				"required":             []string{"decision", "reason"},
				"additionalProperties": false,
			},
		}),
	); err != nil {
		panic(err)
	}
	return true
}

func guardDecisionShape(raw string, in *Input) (*Output, error) {
	if in == nil || !EventContract(in.HookEventName).CanBlock {
		return nil, errors.New("guard_decision requires a blocking event")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("invalid guard verdict")
	}
	fields := make(map[string]string, 2)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, errors.New("invalid guard verdict")
		}
		key, ok := token.(string)
		if !ok || (key != "decision" && key != "reason") {
			return nil, errors.New("invalid guard verdict field")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate guard verdict field")
		}
		token, err = decoder.Token()
		value, ok := token.(string)
		if err != nil || !ok {
			return nil, errors.New("invalid guard verdict value")
		}
		fields[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("invalid guard verdict")
	}
	if decoder.Decode(new(any)) != io.EOF || len(fields) != 2 {
		return nil, errors.New("invalid guard verdict")
	}
	switch fields["decision"] {
	case "allow":
		return &Output{Continue: new(true)}, nil
	case "deny":
		return &Output{Decision: DecisionBlockValue, Reason: fields["reason"]}, nil
	default:
		return nil, errors.New("guard verdict must be allow or deny")
	}
}
