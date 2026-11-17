//go:build docker_agent_no_js_expansion

package js

import (
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestJSExpansionDisabledExpand(t *testing.T) {
	t.Parallel()

	exp := NewJsExpander(environment.NewMapEnvProvider(map[string]string{"USER": "ada", "TOKEN": "s3cret"}))
	for _, tc := range []struct {
		name   string
		text   string
		values map[string]string
		want   string
	}{
		{name: "no placeholder", text: "plain text", want: "plain text"},
		{name: "env", text: "hello ${env.USER}", want: "hello ada"},
		{name: "env with spaces", text: "hello ${ env.USER }", want: "hello ada"},
		{name: "unset env is empty, as with the engine", text: "Bearer ${env.MISSING}", want: "Bearer "},
		{name: "bound value", text: "/ping?who=${who}", values: map[string]string{"who": "you"}, want: "/ping?who=you"},
		{name: "unbound name is kept", text: "${who}", want: "${who}"},
		{name: "expressions are not evaluated", text: "${6 * 7} ${env.USER || 'x'} ${env.USER.toUpperCase()}", want: "${6 * 7} ${env.USER || 'x'} ${env.USER.toUpperCase()}"},
		{name: "values are not expanded again", text: "${v}", values: map[string]string{"v": "${env.TOKEN}"}, want: "${env.TOKEN}"},
		{name: "a placeholder nested in an expression is kept with it", text: `${f({v: "${env.TOKEN}"})} ${env.USER}`, want: `${f({v: "${env.TOKEN}"})} ada`},
		{name: "dollar before a placeholder", text: "$${env.USER}", want: "$ada"},
		{name: "unclosed expression", text: "${env.USER} ${env.USER", want: "ada ${env.USER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, exp.Expand(t.Context(), tc.text, tc.values))
		})
	}
}

func TestJSExpansionDisabledExpandMap(t *testing.T) {
	t.Parallel()

	exp := NewJsExpander(environment.NewMapEnvProvider(map[string]string{"TOKEN": "s3cret"}))
	assert.Nil(t, exp.ExpandMap(t.Context(), nil))
	assert.Equal(t,
		map[string]string{"Authorization": "Bearer s3cret", "X-Static": "v"},
		exp.ExpandMap(t.Context(), map[string]string{"Authorization": "Bearer ${env.TOKEN}", "X-Static": "v"}),
	)
}

// A nil Expander fails where the engine does: fetch, openapi and a2a rely on
// it accepting a nil map only.
func TestJSExpansionDisabledNilExpander(t *testing.T) {
	t.Parallel()

	var exp *Expander
	assert.Nil(t, exp.ExpandMap(t.Context(), nil))
	assert.Nil(t, exp.ExpandCommands(t.Context(), nil))
	assert.Equal(t, "plain", exp.Expand(t.Context(), "plain", nil))
	if runtime.GOOS == "windows" {
		t.Skip("recovered hardware faults can corrupt memory on Windows: golang/go#81238")
	}
	assert.Panics(t, func() { exp.ExpandMap(t.Context(), map[string]string{}) })
	assert.Panics(t, func() { exp.ExpandCommands(t.Context(), types.Commands{}) })
	assert.Panics(t, func() { exp.Expand(t.Context(), "${who}", nil) })
}

func TestJSExpansionDisabledExpandCommands(t *testing.T) {
	t.Parallel()

	exp := NewJsExpander(environment.NewMapEnvProvider(map[string]string{"USER": "ada"}))
	assert.Nil(t, exp.ExpandCommands(t.Context(), nil))
	expanded := exp.ExpandCommands(t.Context(), types.Commands{
		"ask": {Description: "for ${env.USER}", Instruction: "greet ${env.USER}", URL: "https://example.com/${env.USER}", Agent: "specialist"},
	})
	assert.Equal(t, types.Command{Description: "for ada", Instruction: "greet ada", URL: "https://example.com/ada", Agent: "specialist"}, expanded["ask"])
}

// Without the engine the evaluator cannot call tools: the expression stays
// visible instead of silently vanishing.
func TestJSExpansionDisabledEvaluate(t *testing.T) {
	t.Parallel()

	exp := NewEvaluator([]tools.Tool{{Name: "echo"}})
	const input = `${echo({msg: "hi"})} ${args[0]} ${env.USER}`
	assert.Equal(t, input, exp.Evaluate(t.Context(), input, []string{"a"}))
}

func TestJSExpansionDisabledExpandMapFunc(t *testing.T) {
	t.Parallel()

	lookup := func(name string) string { return "<" + strings.ToLower(name) + ">" }
	got := ExpandMapFunc(map[string]string{
		"dot":      "${headers.Authorization}",
		"double":   `${headers["X-Request-Id"]}`,
		"single":   `${ headers['X-Request-Id'] }`,
		"other":    "${env.TOKEN} ${headers.A || 'x'}",
		"nested":   `${f("${headers.A}")}`,
		"rewrites": "${up.A}",
	}, "headers", lookup, func(s string) string { return strings.ReplaceAll(s, "up.", "headers.") })

	assert.Equal(t, map[string]string{
		"dot":      "<authorization>",
		"double":   "<x-request-id>",
		"single":   "<x-request-id>",
		"other":    "${env.TOKEN} ${headers.A || 'x'}",
		"nested":   `${f("${headers.A}")}`,
		"rewrites": "<a>",
	}, got)
}
