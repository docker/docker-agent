//go:build js && wasm && docker_agent_wasm_small

package main

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
)

const smallProfile = true

func TestSmallProfileRejectsOtherProviders(t *testing.T) {
	for _, model := range []string{"anthropic/claude-sonnet-4-6", "google/gemini-2.5-flash", "amazon-bedrock/anthropic.claude-sonnet-4-6"} {
		t.Run(model, func(t *testing.T) {
			yaml := "agents:\n  root:\n    model: " + model + "\n"
			_, err := browserHost.openSession(t.Context(), sessionOptions{YAML: yaml, Env: map[string]string{"ANTHROPIC_API_KEY": "k", "GOOGLE_API_KEY": "k"}})
			require.ErrorContains(t, err, "provider")
		})
	}
}

// What the profile leaves out fails the session instead of being skipped.
func TestSmallProfileRejectsLeftOutFeatures(t *testing.T) {
	h := testHost(&echoToolSet{}, map[string]provider.Provider{"root": newScriptedModel("mock/root")})
	for name, tc := range map[string]struct {
		yaml string
		want string
	}{
		"openapi": {want: `toolset type "openapi"`, yaml: `
agents:
  root:
    model: mock/root
    toolsets:
      - type: openapi
        url: https://example.com/openapi.json
`},
		"code mode": {want: `feature "code_mode"`, yaml: `
agents:
  root:
    model: mock/root
    code_mode_tools: true
`},
		"template hook": {want: `builtin "add_context" is not available`, yaml: `
agents:
  root:
    model: mock/root
    hooks:
      turn_start:
        - type: builtin
          command: add_context
          args: ["Be brief."]
`},
		"historical config version": {want: "unsupported config version", yaml: `
version: "2"
agents:
  root:
    model: mock/root
`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.openSession(t.Context(), sessionOptions{YAML: tc.yaml})
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// Without the JavaScript engine ${env.NAME} still resolves from the session
// env; any other expression stays as text.
func TestSmallProfileExpandsEnvPlaceholdersOnly(t *testing.T) {
	const yaml = `
agents:
  root:
    model: mock/root
    instruction: Hello ${env.WHO}, the answer is ${6 * 7}; secret is [${env.SECRET}].
`
	t.Setenv("SECRET", "from-process")
	model := newScriptedModel("mock/root", textTurn("ok"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: yaml, Env: map[string]string{"WHO": "browser"}})

	_, err := s.send("hi", func(context.Context, map[string]any) {})
	require.NoError(t, err)
	require.Equal(t, chat.MessageRoleSystem, model.lastCall()[0].Role)
	assert.Contains(t, model.lastCall()[0].Content, "Hello browser, the answer is ${6 * 7}; secret is [].")
	assert.Equal(t, "from-process", os.Getenv("SECRET"), "the process env exists but is never consulted")
}

func TestSmallProfileHTTPToolsReachRemoteMocks(t *testing.T) {
	server := mockAPI(t)
	yaml := `
agents:
  root:
    model: mock/root
    toolsets:
      - type: fetch
        allow_private_ips: true
      - type: api
        allow_private_ips: true
        api_config:
          name: ping
          method: GET
          endpoint: ` + server.URL + `/ping?who=${who}
          headers:
            Authorization: Bearer ${env.TOKEN}
          args:
            who:
              type: string
`
	model := newScriptedModel("mock/root",
		toolTurn("fetch", `{"urls":["`+server.URL+`/ping?who=fetch"]}`),
		toolTurn("ping", `{"who":"api"}`),
		textTurn("done"))
	s := openTestSession(t, testHost(&echoToolSet{}, map[string]provider.Provider{"root": model}), sessionOptions{YAML: yaml, Env: map[string]string{"TOKEN": "t"}, AutoApprove: true})

	var c collectingEmitter
	_, err := s.send("ping everything", c.emit)
	require.NoError(t, err)
	results := c.find("tool_result")
	require.Len(t, results, 2)
	assert.Contains(t, results[0]["output"], `"pong":"fetch"`)
	assert.Contains(t, results[1]["output"], `"pong":"api","auth":"Bearer t"`)
}
