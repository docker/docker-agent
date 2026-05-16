package hooks

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// fakeClient lets the tests exercise the model factory without standing
// up a real provider. It records the most recent call and returns a
// canned reply (or error).
type fakeClient struct {
	gotModel  string
	gotSystem string
	gotUser   string
	gotSchema *latest.StructuredOutput
	reply     string
	err       error
	calls     int
}

func (f *fakeClient) Ask(_ context.Context, modelSpec, system, user string, schema *latest.StructuredOutput) (string, error) {
	f.calls++
	f.gotModel = modelSpec
	f.gotSystem = system
	f.gotUser = user
	f.gotSchema = schema
	return f.reply, f.err
}

// runModelHook is a small helper that builds a Handler via
// NewModelFactory and dispatches once. It returns the handler result
// plus any factory/run error so tests can assert on either path.
func runModelHook(t *testing.T, client ModelClient, h Hook, in *Input) (HandlerResult, error) {
	t.Helper()
	factory := NewModelFactory(client)
	handler, err := factory(HandlerEnv{}, h)
	if err != nil {
		return HandlerResult{}, err
	}
	body, err := in.ToJSON()
	require.NoError(t, err)
	return handler.Run(t.Context(), body)
}

// TestNewModelFactoryRejectsMissingFields documents the up-front
// validation: a Hook lacking Model or Prompt must fail at factory
// build, not at first dispatch. This keeps misconfiguration loud at
// startup rather than discovering it on the first user request.
func TestNewModelFactoryRejectsMissingFields(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		hook Hook
		want string
	}{
		{
			"missing model",
			Hook{Type: HookTypeModel, Prompt: "hi"},
			"non-empty model",
		},
		{
			"missing prompt",
			Hook{Type: HookTypeModel, Model: "openai/gpt-4o-mini"},
			"non-empty prompt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			factory := NewModelFactory(&fakeClient{})
			_, err := factory(HandlerEnv{}, tc.hook)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestNewModelFactoryRejectsBadTemplate verifies that a syntactically
// invalid Go template surfaces at factory time so a typo doesn't slip
// through to the first dispatch.
func TestNewModelFactoryRejectsBadTemplate(t *testing.T) {
	t.Parallel()

	factory := NewModelFactory(&fakeClient{})
	_, err := factory(HandlerEnv{}, Hook{
		Type:   HookTypeModel,
		Model:  "openai/gpt-4o-mini",
		Prompt: "{{ .ToolName ", // unterminated action
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse prompt template")
}

// TestNewModelFactoryRejectsUnknownSchema pins the registry contract:
// referencing a schema that nobody registered must error at factory
// time, not silently fall back to additional_context.
func TestNewModelFactoryRejectsUnknownSchema(t *testing.T) {
	t.Parallel()

	factory := NewModelFactory(&fakeClient{})
	_, err := factory(HandlerEnv{}, Hook{
		Type:   HookTypeModel,
		Model:  "openai/gpt-4o-mini",
		Prompt: "hi",
		Schema: "no_such_shape",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown schema")
}

// TestModelHandlerRendersPromptAndForwardsContext exercises the happy
// path for the default (free-form) shape: the prompt template sees the
// Input fields, toJSON works, and the model's reply lands as
// AdditionalContext on a turn_start event.
func TestModelHandlerRendersPromptAndForwardsContext(t *testing.T) {
	t.Parallel()

	client := &fakeClient{reply: "Be careful with /etc."}
	res, err := runModelHook(t, client, Hook{
		Type:   HookTypeModel,
		Model:  "openai/gpt-4o-mini",
		Prompt: "Tool: {{ .ToolName }}\nArgs: {{ .ToolInput | toJSON }}",
	}, &Input{
		HookEventName: EventTurnStart,
		ToolName:      "shell",
		ToolInput:     map[string]any{"cmd": "ls"},
	})
	require.NoError(t, err)
	require.NotNil(t, res.Output)
	require.NotNil(t, res.Output.HookSpecificOutput)

	assert.Equal(t, EventTurnStart, res.Output.HookSpecificOutput.HookEventName,
		"default shape must echo the input event so the executor routes the context correctly")
	assert.Equal(t, "Be careful with /etc.", res.Output.HookSpecificOutput.AdditionalContext)

	// Verify the template rendered with the Input fields and toJSON.
	assert.Equal(t, "openai/gpt-4o-mini", client.gotModel)
	assert.Contains(t, client.gotUser, "Tool: shell")
	assert.Contains(t, client.gotUser, `"cmd":"ls"`)
	assert.Nil(t, client.gotSchema, "default (free-form) shape must NOT request structured output")
}

// TestModelHandlerForwardsStructuredOutputSchema checks that the
// schema name on the Hook is resolved to the registered
// [latest.StructuredOutput] and threaded to the client. Providers that
// honor structured output get a strict contract; others ignore it and
// the lenient parser still works.
func TestModelHandlerForwardsStructuredOutputSchema(t *testing.T) {
	t.Parallel()

	client := &fakeClient{reply: `{"decision":"allow","reason":"safe"}`}
	_, err := runModelHook(t, client, Hook{
		Type:   HookTypeModel,
		Model:  "openai/gpt-4o-mini",
		Prompt: "Judge: {{ .ToolName }}",
		Schema: ShapePreToolUseDecision,
	}, &Input{
		HookEventName: EventPreToolUse,
		ToolName:      "shell",
	})
	require.NoError(t, err)
	require.NotNil(t, client.gotSchema, "named schema must be forwarded to the ModelClient")
	assert.Equal(t, "pre_tool_use_decision", client.gotSchema.Name)
	assert.True(t, client.gotSchema.Strict)
}

// TestModelHandlerPropagatesClientErrors documents the failure
// contract: a non-nil error from the ModelClient (auth, rate limit,
// timeout, ...) MUST propagate through Run so the executor's
// fail-closed semantics deny PreToolUse calls. The shape never sees
// the partial reply.
func TestModelHandlerPropagatesClientErrors(t *testing.T) {
	t.Parallel()

	client := &fakeClient{err: errors.New("rate limit")}
	_, err := runModelHook(t, client, Hook{
		Type:   HookTypeModel,
		Model:  "openai/gpt-4o-mini",
		Prompt: "hi",
	}, &Input{HookEventName: EventTurnStart})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rate limit")
}

// TestPreToolUseDecisionShape table-tests the parser for the
// well-known judge shape. It must accept clean JSON, reject empty /
// malformed input (so the executor fails closed), tolerate markdown
// fences, and handle each verdict including case-insensitivity.
func TestPreToolUseDecisionShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		raw         string
		wantOK      bool
		wantVerdict Decision
	}{
		{"clean allow", `{"decision":"allow","reason":"safe"}`, true, DecisionAllow},
		{"clean deny", `{"decision":"deny","reason":"rm -rf"}`, true, DecisionDeny},
		{"clean ask", `{"decision":"ask","reason":"unclear"}`, true, DecisionAsk},
		{"uppercase ALLOW", `{"decision":"ALLOW","reason":"x"}`, true, DecisionAllow},
		{"json in fence", "```json\n{\"decision\":\"ask\",\"reason\":\"x\"}\n```", true, DecisionAsk},
		{"json with prose", "Sure!\n{\"decision\":\"deny\",\"reason\":\"x\"}\nhope this helps", true, DecisionDeny},
		{"empty", "", false, ""},
		{"no json", "I refuse to answer.", false, ""},
		{"bad decision", `{"decision":"maybe","reason":"x"}`, false, ""},
		{"not json", `{decision: allow}`, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := preToolUseDecisionShape(tc.raw, &Input{HookEventName: EventPreToolUse})
			if !tc.wantOK {
				require.Error(t, err, "the executor relies on this error to fail closed")
				assert.Nil(t, out)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, out)
			require.NotNil(t, out.HookSpecificOutput)
			assert.Equal(t, EventPreToolUse, out.HookSpecificOutput.HookEventName)
			assert.Equal(t, tc.wantVerdict, out.HookSpecificOutput.PermissionDecision)
			assert.NotEmpty(t, out.HookSpecificOutput.PermissionDecisionReason,
				"shape must always populate a reason (default 'no reason provided')")
			assert.Contains(t, out.SystemMessage, string(tc.wantVerdict),
				"system_message must surface the verdict to the UI")
		})
	}
}

// TestPreToolUseDecisionShapeIsRegistered documents the package-level
// init contract: the shape and its strict JSON schema MUST be
// available immediately, without any caller-side registration. This
// test would fail loudly if a future refactor moved registration into
// an opt-in helper.
func TestPreToolUseDecisionShapeIsRegistered(t *testing.T) {
	t.Parallel()

	shape, ok := lookupShape(ShapePreToolUseDecision)
	require.True(t, ok, "%q must be auto-registered by package init", ShapePreToolUseDecision)
	require.NotNil(t, shape)

	schema := lookupSchema(ShapePreToolUseDecision)
	require.NotNil(t, schema, "well-known shape must have a paired structured-output schema")
	assert.True(t, schema.Strict)
}

// TestLookupShapeDefault pins that the empty schema name maps to the
// free-form additional_context shape with no structured-output
// schema, so a `type: model` hook without a Schema field works
// out of the box.
func TestLookupShapeDefault(t *testing.T) {
	t.Parallel()

	shape, ok := lookupShape("")
	require.True(t, ok)
	require.NotNil(t, shape)
	assert.Nil(t, lookupSchema(""), "default shape must NOT request structured output")
}

// TestRegisterResponseShapeRejectsBadInput pins the public contract of
// the registration helpers — empty names and nil shapes/schemas must
// be rejected so a typo fails loudly at startup.
func TestRegisterResponseShapeRejectsBadInput(t *testing.T) {
	t.Parallel()

	require.Error(t, RegisterResponseShape("", defaultShape))
	require.Error(t, RegisterResponseShape("nil-shape", nil))
	require.Error(t, RegisterResponseSchema("", &latest.StructuredOutput{Name: "x"}))
}

// TestRegisterResponseShapeRoundTrip exercises the public extension
// point: an external caller can register a custom shape + schema
// pair, reference it from a `type: model` hook via Hook.Schema, and
// have it drive the aggregated Result. This is the contract that
// makes pkg/hooks reusable beyond docker-agent's built-in shapes.
func TestRegisterResponseShapeRoundTrip(t *testing.T) {
	t.Parallel()

	const name = "test_round_trip_shape"
	require.NoError(t, RegisterResponseShape(name, func(raw string, in *Input) (*Output, error) {
		return NewAdditionalContextOutput(in.HookEventName, "shape saw: "+raw), nil
	}))
	require.NoError(t, RegisterResponseSchema(name, &latest.StructuredOutput{Name: name, Strict: true}))

	client := &fakeClient{reply: "hello"}
	res, err := runModelHook(t, client, Hook{
		Type:   HookTypeModel,
		Model:  "openai/gpt-4o-mini",
		Prompt: "hi",
		Schema: name,
	}, &Input{HookEventName: EventTurnStart})
	require.NoError(t, err)
	require.NotNil(t, res.Output)
	require.NotNil(t, res.Output.HookSpecificOutput)
	assert.Equal(t, "shape saw: hello", res.Output.HookSpecificOutput.AdditionalContext)
	require.NotNil(t, client.gotSchema, "registered schema must be threaded to the ModelClient")
	assert.Equal(t, name, client.gotSchema.Name)
}

// TestNilModelClientFailsAtFirstDispatch documents the lazy-failure
// behavior of NewModelFactory(nil): the factory builds successfully so
// a runtime can register it before credentials are available, but the
// first dispatch errors with a clear message. PreToolUse then fails
// closed.
func TestNilModelClientFailsAtFirstDispatch(t *testing.T) {
	t.Parallel()

	_, err := runModelHook(t, nil, Hook{
		Type:   HookTypeModel,
		Model:  "openai/gpt-4o-mini",
		Prompt: "hi",
	}, &Input{HookEventName: EventTurnStart})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no ModelClient configured")
}

// TestPromptTemplateTruncateHelper covers the small helper exposed to
// prompts: long inputs can be capped with `{{ truncate 16 .Cwd }}` to
// keep prompt length bounded. Also documents that n<=0 is a no-op.
func TestPromptTemplateTruncateHelper(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", 50)
	client := &fakeClient{reply: "ok"}
	_, err := runModelHook(t, client, Hook{
		Type:   HookTypeModel,
		Model:  "openai/gpt-4o-mini",
		Prompt: "cwd: {{ truncate 16 .Cwd }}",
	}, &Input{HookEventName: EventTurnStart, Cwd: long})
	require.NoError(t, err)
	assert.Contains(t, client.gotUser, "cwd: aaaaaaaaaaaaaaaa…")
}
