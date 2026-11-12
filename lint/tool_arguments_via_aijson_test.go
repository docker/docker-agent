package main

import (
	"testing"

	"github.com/dgageot/rubocop-go/coptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolArgumentsViaAIJSONFlagsJSONUnmarshal(t *testing.T) {
	t.Parallel()
	src := `package p
import (
	"encoding/json"
	"github.com/docker/docker-agent/pkg/tools"
)
func f(toolCall tools.ToolCall) error {
	var args struct{ Name string }
	return json.Unmarshal([]byte(toolCall.Function.Arguments), &args)
}
`
	offenses := coptest.Run(t, ToolArgumentsViaAIJSON, src)
	require.Len(t, offenses, 1)
	assert.Equal(t, "Lint/ToolArgumentsViaAIJSON", offenses[0].CopName)
	assert.Contains(t, offenses[0].Message, "tools.UnmarshalToolArguments")
}

func TestToolArgumentsViaAIJSONFlagsAliasedImports(t *testing.T) {
	t.Parallel()
	src := `package p
import (
	stdjson "encoding/json"
	agenttools "github.com/docker/docker-agent/pkg/tools"
)
func f(tc agenttools.ToolCall) error {
	var args map[string]any
	return stdjson.Unmarshal([]byte(tc.Function.Arguments), &args)
}
`
	assert.Len(t, coptest.Run(t, ToolArgumentsViaAIJSON, src), 1)
}

func TestToolArgumentsViaAIJSONAllowsSharedDecoder(t *testing.T) {
	t.Parallel()
	src := `package p
import (
	"context"
	"github.com/docker/docker-agent/pkg/tools"
)
func f(ctx context.Context, toolCall tools.ToolCall) error {
	var args struct{ Name string }
	return tools.UnmarshalToolArguments(ctx, toolCall, &args)
}
`
	assert.Empty(t, coptest.Run(t, ToolArgumentsViaAIJSON, src))
}

func TestToolArgumentsViaAIJSONFlagsFunctionArgumentsShape(t *testing.T) {
	t.Parallel()
	src := `package p
import "encoding/json"
type functionCall struct{ Arguments string }
type call struct{ Function functionCall }
func f(call call) error {
	var args map[string]any
	return json.Unmarshal([]byte(call.Function.Arguments), &args)
}
`
	assert.Len(t, coptest.Run(t, ToolArgumentsViaAIJSON, src), 1)
}

func TestToolArgumentsViaAIJSONIgnoresOrdinaryJSON(t *testing.T) {
	t.Parallel()
	src := `package p
import "encoding/json"
func f(raw string) error {
	var value map[string]any
	return json.Unmarshal([]byte(raw), &value)
}
`
	assert.Empty(t, coptest.Run(t, ToolArgumentsViaAIJSON, src))
}
