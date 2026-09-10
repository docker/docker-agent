package teamloader

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestEnvExpander(t *testing.T) {
	t.Parallel()

	env := environment.NewMapEnvProvider(map[string]string{"REGION": "eu"})
	exp := newEnvExpander(env)

	assert.Equal(t, "plain", exp.Expand(t.Context(), "plain", nil))
	assert.Equal(t, "in eu", exp.Expand(t.Context(), "in ${env.REGION}", nil))
	assert.Equal(t, "in ${env.MISSING}", exp.Expand(t.Context(), "in ${env.MISSING}", nil), "unset vars stay visible")
	assert.Equal(t, "hi bob", exp.Expand(t.Context(), "hi ${name}", map[string]string{"name": "bob"}))
	assert.Equal(t, "hi ${REGION}", exp.Expand(t.Context(), "hi ${REGION}", nil), "bare names only resolve bound values")
	assert.Equal(t, "${env.REGION || 'x'}", exp.Expand(t.Context(), "${env.REGION || 'x'}", nil), "JavaScript is left for pkg/js")

	cmds := exp.ExpandCommands(t.Context(), types.Commands{"c": {Description: "d ${env.REGION}", Instruction: "i", Agent: "root"}})
	assert.Equal(t, "d eu", cmds["c"].Description)
	assert.Equal(t, "root", cmds["c"].Agent)
	assert.Nil(t, exp.ExpandCommands(t.Context(), nil))
}

func TestEnvExpanderMap(t *testing.T) {
	t.Parallel()
	exp := NewEnvExpander(environment.NewMapEnvProvider(map[string]string{"TOKEN": "secret"}))
	input := map[string]string{
		"Authorization": "Bearer ${env.TOKEN}",
		"Unknown":       "${env.MISSING}",
		"JavaScript":    "${1 + 1}",
	}
	got := exp.ExpandMap(t.Context(), input)
	assert.Equal(t, "Bearer secret", got["Authorization"])
	assert.Equal(t, "Bearer ${env.TOKEN}", input["Authorization"])
	assert.Equal(t, "${env.MISSING}", got["Unknown"])
	assert.Equal(t, "${1 + 1}", got["JavaScript"])
	assert.Empty(t, exp.ExpandMap(t.Context(), nil))
}
