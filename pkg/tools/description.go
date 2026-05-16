package tools

import (
	"encoding/json"
)

const (
	// DescriptionParam is the parameter name for the description
	DescriptionParam = "description"
)

// AddDescriptionParameter adds a "description" parameter to tools that have
// AddDescriptionParameter set to true. This allows the LLM to provide context
// about what it's doing with each tool call.
func AddDescriptionParameter(toolList []Tool) []Tool {
	result := make([]Tool, len(toolList))
	for i, tool := range toolList {
		result[i] = addDescriptionParam(tool)
	}
	return result
}

func addDescriptionParam(tool Tool) Tool {
	if !tool.AddDescriptionParameter {
		return tool
	}

	schema, err := SchemaToMap(tool.Parameters)
	if err != nil {
		return tool
	}

	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		properties = make(map[string]any)
		schema["properties"] = properties
	}

	properties[DescriptionParam] = map[string]any{
		"type":        "string",
		"description": "Brief description of this call",
	}

	tool.Parameters = schema
	return tool
}

// ExtractDescription extracts the description from tool call arguments.
func ExtractDescription(arguments string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return ""
	}

	if desc, ok := args[DescriptionParam].(string); ok {
		return desc
	}
	return ""
}
