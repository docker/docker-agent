package leantui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestRetractedToolCallsAreRemoved(t *testing.T) {
	t.Parallel()
	m := &model{screen: ui.NewScreen("", "", "")}
	for _, id := range []string{"declined", "kept"} {
		m.handleEvent(t.Context(), runtime.PartialToolCall(tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: "shell"}}, tools.Tool{}, "root"))
	}

	m.handleEvent(t.Context(), runtime.ToolCallsRetracted([]string{"declined"}, "root"))

	assert.Nil(t, m.screen.Transcript.Tool("declined"))
	assert.NotNil(t, m.screen.Transcript.Tool("kept"))
}
