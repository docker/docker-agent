package builtins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/docker/docker-agent/pkg/hooks"
)

// TransformJSON keeps selected top-level fields of a JSON tool response.
const TransformJSON = "transform_json"

func transformJSON(_ context.Context, in *hooks.Input, args []string) (*hooks.Output, error) {
	if in == nil || in.HookEventName != hooks.EventToolResponseTransform || in.ToolError || len(args) == 0 {
		return nil, nil
	}
	raw, ok := in.ToolResponse.(string)
	if !ok {
		return nil, errors.New("transform_json: expected JSON text")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return nil, fmt.Errorf("transform_json: decode response: %w", err)
	}
	if fields == nil {
		return nil, errors.New("transform_json: expected a JSON object")
	}
	selected := make(map[string]json.RawMessage, len(args))
	for _, key := range args {
		if value, exists := fields[key]; exists {
			selected[key] = value
		}
	}
	data, err := json.Marshal(selected)
	if err != nil {
		return nil, fmt.Errorf("transform_json: encode response: %w", err)
	}
	updated := string(data)
	return &hooks.Output{HookSpecificOutput: &hooks.HookSpecificOutput{
		HookEventName: in.HookEventName, UpdatedToolResponse: &updated,
	}}, nil
}
