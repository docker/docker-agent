//go:build js && wasm

package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/model/provider"
)

// The browser wrapper doesn't pass the three session options. Set them on the
// Go session to pin the runtime behavior implemented by portable sessions.
func TestSmallProfileSessionLimitsReference(t *testing.T) {
	var fixture struct {
		Source               string `json:"source"`
		MaxToolResultTokens  int    `json:"max_tool_result_tokens"`
		Stored               string `json:"stored_result"`
		MaxOldToolCallTokens int    `json:"max_old_tool_call_tokens"`
		Projected            string `json:"projected_result"`
		Stop                 string `json:"stop"`
	}
	data, err := os.ReadFile("portable/testdata/session-limits.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &fixture))
	args, err := json.Marshal(map[string]string{"text": strings.TrimPrefix(fixture.Source, "echo: ")})
	require.NoError(t, err)
	model := newScriptedModel("mock/root", toolTurn("echo", string(args)), textTurn("done"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML, AutoApprove: true})
	sess := s.chat.Conversation()
	sess.MaxIterations, sess.MaxToolResultTokens, sess.MaxOldToolCallTokens = 1, fixture.MaxToolResultTokens, fixture.MaxOldToolCallTokens

	var c collectingEmitter
	result, err := s.send("first", c.emit)
	require.NoError(t, err)
	require.Equal(t, fixture.Source, c.find("tool_result")[0]["output"])
	require.Equal(t, fixture.Stored, sess.Messages[len(sess.Messages)-2].Message.Message.Content)
	require.Equal(t, fixture.Stop, result["message"].(map[string]any)["content"])
	require.Equal(t, fixture.Stop, c.find("delta")[0]["content"])
	require.Equal(t, "stop", c.find("finish")[0]["reason"])
	require.Equal(t, 1, model.callCount())
	_, err = s.send("again", c.emit)
	require.NoError(t, err)
	last := model.lastCall()
	require.Equal(t, fixture.Projected, last[len(last)-3].Content)
	require.Equal(t, fixture.Stop, last[len(last)-2].Content)
}
