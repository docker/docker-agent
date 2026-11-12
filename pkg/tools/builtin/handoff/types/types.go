// Package types defines the shared wire contracts for the handoff tools.
package types

const ToolNameHandoff = "handoff"

type Args struct {
	Agent string `json:"agent" jsonschema:"The name of the agent to hand off the conversation to."`
}
