package tools

import (
	"bytes"
	"encoding/json"
)

// MarshalJSON preserves generated argument declaration order in exported tools.
func (t Tool) MarshalJSON() ([]byte, error) {
	type wireTool Tool
	wire := wireTool(t)
	for _, schema := range []*any{&wire.Parameters, &wire.OutputSchema} {
		if m, ok := (*schema).(map[string]any); ok {
			*schema = schemaWithOrder(m)
		}
	}
	return json.Marshal(wire)
}

// UnmarshalJSON retains schema property order when tools cross API boundaries.
func (t *Tool) UnmarshalJSON(data []byte) error {
	type wireTool Tool
	var wire wireTool
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	var schemas struct {
		Parameters   json.RawMessage `json:"parameters"`
		OutputSchema json.RawMessage `json:"outputSchema"`
	}
	if err := json.Unmarshal(data, &schemas); err != nil {
		return err
	}
	for _, field := range []struct {
		data        json.RawMessage
		destination *any
	}{{schemas.Parameters, &wire.Parameters}, {schemas.OutputSchema, &wire.OutputSchema}} {
		if len(field.data) == 0 || bytes.Equal(field.data, []byte("null")) {
			continue
		}
		schema, err := decodeSchema(field.data)
		if err != nil {
			return err
		}
		*field.destination = schemaWithOrder(schema)
	}
	*t = Tool(wire)
	return nil
}
