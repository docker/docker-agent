//go:build js && wasm && docker_agent_wasm_small

package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/model/provider"
)

func TestSmallProfileRejectsSkills(t *testing.T) {
	h := testHost(&echoToolSet{}, map[string]provider.Provider{"root": newScriptedModel("mock/root")})
	for name, skills := range map[string]string{
		"workspace": "true",
		"inline": `
      - name: example
        description: An example skill.
        instructions: Be brief.
`,
	} {
		t.Run(name, func(t *testing.T) {
			yaml := "agents:\n  root:\n    model: mock/root\n    skills: " + skills + "\n"
			_, err := h.openSession(t.Context(), sessionOptions{YAML: yaml})
			require.ErrorContains(t, err, `feature "skills"`)
		})
	}
}
