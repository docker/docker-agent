package evaluation

import (
	"encoding/json"

	"github.com/docker/docker-agent/pkg/tools/builtin/structuredoutput"
)

// acceptedStructuredOutputs correlates successful internal output responses,
// never treating rejected calls or arbitrary tool JSON as final answers.
func acceptedStructuredOutputs(events []map[string]any) map[int]string {
	type callKey struct {
		id    string
		agent string
	}
	calls := make(map[callKey]bool)
	outputs := make(map[int]string)
	for index, event := range events {
		if event["type"] == "tool_call" {
			call, _ := event["tool_call"].(map[string]any)
			function, _ := call["function"].(map[string]any)
			id, _ := call["id"].(string)
			if function["name"] == structuredoutput.ToolName && id != "" {
				agent, _ := event["agent_name"].(string)
				calls[callKey{id: id, agent: agent}] = true
			}
			continue
		}
		if event["type"] != "tool_call_response" {
			continue
		}
		id, _ := event["tool_call_id"].(string)
		agent, _ := event["agent_name"].(string)
		key := callKey{id: id, agent: agent}
		if !calls[key] {
			continue
		}
		definition, _ := event["tool_definition"].(map[string]any)
		result, _ := event["result"].(map[string]any)
		if definition["name"] != structuredoutput.ToolName || result == nil {
			continue
		}
		delete(calls, key)
		if result["isError"] == true {
			continue
		}
		response, _ := event["response"].(string)
		if json.Valid([]byte(response)) {
			outputs[index] = response
		}
	}
	return outputs
}
