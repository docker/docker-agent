package builtins

import (
	"encoding/json"
	"testing"

	"github.com/docker/portcullis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/internal/portcullistest"
	"github.com/docker/docker-agent/pkg/tools"
)

func fakeGitHubPAT() string {
	return portcullistest.FakeGitHubPAT("cxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
}

// TestRedactSecretsScrubsTopLevelStringValue: a recognised secret in
// a top-level string argument is replaced and ONLY the rewritten key
// is emitted in UpdatedInput. Both the auto-injected
// tool_input_transform leg and an explicit legacy pre_tool_use entry
// scrub arguments, echoing the dispatching event name back.
func TestRedactSecretsScrubsTopLevelStringValue(t *testing.T) {
	t.Parallel()

	for _, event := range []hooks.EventType{hooks.EventToolInputTransform, hooks.EventPreToolUse} {
		t.Run(string(event), func(t *testing.T) {
			t.Parallel()

			secret := fakeGitHubPAT()

			in := &hooks.Input{
				HookEventName: event,
				ToolName:      "shell",
				ToolInput: map[string]any{
					"command": "curl -H 'Authorization: token " + secret + "' https://api.github.com",
					"timeout": 30,
				},
			}

			out, err := redactSecrets(t.Context(), in, nil)
			require.NoError(t, err)
			require.NotNil(t, out, "must return Output when redaction happened")
			require.NotNil(t, out.HookSpecificOutput)

			updated := out.HookSpecificOutput.UpdatedInput
			cmd, ok := updated["command"].(string)
			require.True(t, ok, "changed key must appear in UpdatedInput")
			assert.NotContains(t, cmd, secret, "raw secret must be gone")
			assert.Contains(t, cmd, portcullis.Marker)
			assert.NotContains(t, updated, "timeout",
				"unchanged keys need no patch")
			assert.Equal(t, event, out.HookSpecificOutput.HookEventName)
		})
	}
}

// TestRedactSecretsReturnsNilWhenNothingChanged: clean tool calls
// take the no-overhead path (executor treats nil Output as "this
// builtin contributed nothing").
func TestRedactSecretsReturnsNilWhenNothingChanged(t *testing.T) {
	t.Parallel()

	out, err := redactSecrets(t.Context(), &hooks.Input{
		HookEventName: hooks.EventPreToolUse,
		ToolName:      "shell",
		ToolInput:     map[string]any{"command": "ls -la", "timeout": 30},
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, out, "no secrets ⇒ no Output")
}

// TestRedactSecretsHandlesNilAndEmptyInputs: missing/empty ToolInput
// must not panic and must not fabricate an UpdatedInput.
func TestRedactSecretsHandlesNilAndEmptyInputs(t *testing.T) {
	t.Parallel()

	for _, in := range []*hooks.Input{
		nil,
		{HookEventName: hooks.EventPreToolUse},
		{HookEventName: hooks.EventPreToolUse, ToolInput: map[string]any{}},
	} {
		out, err := redactSecrets(t.Context(), in, nil)
		require.NoError(t, err)
		assert.Nil(t, out)
	}
}

// TestRedactSecretsWalksNestedStructures: secrets nested inside
// map[string]any / []any payloads (the shape MCP and OpenAPI bridges
// pass through) are caught alongside top-level values. The rebuilt
// nested container preserves unchanged sibling values — the
// "only changed keys" rule applies at the TOP level only, since the
// executor's maps.Copy is shallow.
func TestRedactSecretsWalksNestedStructures(t *testing.T) {
	t.Parallel()

	const secret = "dckr_pat_" + "AAAAAAAAAAAAAAAAAAAAAAAAAAA"

	in := &hooks.Input{
		HookEventName: hooks.EventPreToolUse,
		ToolName:      "http_request",
		ToolInput: map[string]any{
			"headers": map[string]any{
				"Authorization": "Bearer " + secret,
				"Accept":        "application/json",
			},
			"tags": []any{"prod", secret, 42},
		},
	}

	out, err := redactSecrets(t.Context(), in, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	updated := out.HookSpecificOutput.UpdatedInput

	headers := updated["headers"].(map[string]any)
	auth, _ := headers["Authorization"].(string)
	assert.NotContains(t, auth, secret)
	assert.Contains(t, auth, portcullis.Marker)
	assert.Equal(t, "application/json", headers["Accept"], "non-secret header preserved")

	tags := updated["tags"].([]any)
	require.Len(t, tags, 3)
	assert.Equal(t, "prod", tags[0])
	tag1, _ := tags[1].(string)
	assert.NotContains(t, tag1, secret)
	assert.Contains(t, tag1, portcullis.Marker)
	assert.Equal(t, 42, tags[2])
}

// TestRedactSecretsIsRegistered: the builtin is reachable via
// hooks.Registry (the path YAML config takes), not just by direct
// call. Regressions usually mean a missing RegisterBuiltin line.
func TestRedactSecretsIsRegistered(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	require.NoError(t, Register(reg))

	handler, ok := reg.LookupBuiltin(RedactSecrets)
	require.Truef(t, ok, "builtin %q must be registered", RedactSecrets)

	secret := fakeGitHubPAT()
	out, err := handler(t.Context(), &hooks.Input{
		HookEventName: hooks.EventPreToolUse,
		ToolName:      "shell",
		ToolInput:     map[string]any{"cmd": secret},
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	cmd, _ := out.HookSpecificOutput.UpdatedInput["cmd"].(string)
	assert.NotContains(t, cmd, secret)
}

// TestApplyAgentDefaultsInjectsRedactSecrets: setting the agent flag
// must materialise hook entries for ALL THREE legs of the
// redact_secrets feature — tool_input_transform (tool args, before
// approval), before_llm_call (outgoing chat), and
// tool_response_transform (tool output) — each pointing at the same
// redact_secrets builtin. Nothing is injected on pre_tool_use.
func TestApplyAgentDefaultsInjectsRedactSecrets(t *testing.T) {
	t.Parallel()

	cfg := ApplyAgentDefaults(nil, AgentDefaults{RedactSecrets: true})
	require.NotNil(t, cfg)

	// Leg 1: tool_input_transform, wildcard matcher.
	require.Len(t, cfg.ToolInputTransform, 1)
	assert.Equal(t, "*", cfg.ToolInputTransform[0].Matcher)
	require.Len(t, cfg.ToolInputTransform[0].Hooks, 1)
	assert.Equal(t, hooks.HookTypeBuiltin, cfg.ToolInputTransform[0].Hooks[0].Type)
	assert.Equal(t, RedactSecrets, cfg.ToolInputTransform[0].Hooks[0].Command)
	assert.Empty(t, cfg.PreToolUse, "argument scrubbing moved off pre_tool_use")

	// Leg 2: before_llm_call, flat (event is not tool-scoped).
	require.Len(t, cfg.BeforeLLMCall, 1)
	assert.Equal(t, hooks.HookTypeBuiltin, cfg.BeforeLLMCall[0].Type)
	assert.Equal(t, RedactSecrets, cfg.BeforeLLMCall[0].Command)

	// Leg 3: tool_response_transform, wildcard matcher. The always-on
	// large-result limiter is also installed on this event, so find the
	// redact_secrets entry by command rather than relying on position.
	var redactTransform *hooks.MatcherConfig
	for i := range cfg.ToolResponseTransform {
		if len(cfg.ToolResponseTransform[i].Hooks) == 1 && cfg.ToolResponseTransform[i].Hooks[0].Command == RedactSecrets {
			redactTransform = &cfg.ToolResponseTransform[i]
		}
	}
	require.NotNil(t, redactTransform)
	assert.Equal(t, "*", redactTransform.Matcher)
	require.Len(t, redactTransform.Hooks, 1)
	assert.Equal(t, hooks.HookTypeBuiltin, redactTransform.Hooks[0].Type)
	assert.Equal(t, RedactSecrets, redactTransform.Hooks[0].Command)
}

// TestRedactSecretsScrubsOutgoingMessages exercises the
// before_llm_call leg: the same builtin, dispatched on a
// before_llm_call input carrying a [chat.Message] slice, scrubs every
// text-bearing field and returns the rewrite via
// HookSpecificOutput.UpdatedMessages. Returning UpdatedMessages (and
// not UpdatedInput) is critical because the runtime swaps the slice
// before the model call — a nil here means "don't bother, the
// originals are clean".
func TestRedactSecretsScrubsOutgoingMessages(t *testing.T) {
	t.Parallel()

	secret := fakeGitHubPAT()
	in := &hooks.Input{
		HookEventName: hooks.EventBeforeLLMCall,
		Messages: []chat.Message{
			{Role: chat.MessageRoleUser, Content: "the token is " + secret},
			{Role: chat.MessageRoleAssistant, Content: "never mention secrets"},
		},
	}

	out, err := redactSecrets(t.Context(), in, nil)
	require.NoError(t, err)
	require.NotNil(t, out, "a hit must produce a rewrite")
	require.NotNil(t, out.HookSpecificOutput)
	assert.Equal(t, hooks.EventBeforeLLMCall, out.HookSpecificOutput.HookEventName)

	rewritten := out.HookSpecificOutput.UpdatedMessages
	require.Len(t, rewritten, 2, "rewrite must cover the full slice, not just the dirty entries")
	assert.NotContains(t, rewritten[0].Content, secret)
	assert.Contains(t, rewritten[0].Content, portcullis.Marker)
	assert.Equal(t, "never mention secrets", rewritten[1].Content,
		"clean messages must pass through verbatim")
}

// TestRedactSecretsBeforeLLMCallNoOpOnCleanMessages: a clean
// conversation must short-circuit and return nil so the runtime takes
// the cheap fast-path through the aggregate.
func TestRedactSecretsBeforeLLMCallNoOpOnCleanMessages(t *testing.T) {
	t.Parallel()

	in := &hooks.Input{
		HookEventName: hooks.EventBeforeLLMCall,
		Messages: []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hello"},
			{Role: chat.MessageRoleAssistant, Content: "how can I help"},
		},
	}

	out, err := redactSecrets(t.Context(), in, nil)
	require.NoError(t, err)
	assert.Nil(t, out, "clean conversation ⇒ nil Output")
}

// TestRedactSecretsBeforeLLMCallScrubsToolCallArguments: a dirty
// tool-call argument blob inside a prior assistant message gets
// rewritten too — the LLM's own tool-call history is a leak vector
// when the model decided to echo a credential into the JSON args.
func TestRedactSecretsBeforeLLMCallScrubsToolCallArguments(t *testing.T) {
	t.Parallel()

	secret := fakeGitHubPAT()
	in := &hooks.Input{
		HookEventName: hooks.EventBeforeLLMCall,
		Messages: []chat.Message{
			{
				Role: chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{
					{
						ID: "c1",
						Function: tools.FunctionCall{
							Name:      "shell",
							Arguments: `{"cmd":"curl -H 'Authorization: ` + secret + `'"}`,
						},
					},
				},
			},
		},
	}

	out, err := redactSecrets(t.Context(), in, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	rewritten := out.HookSpecificOutput.UpdatedMessages
	require.Len(t, rewritten, 1)
	require.Len(t, rewritten[0].ToolCalls, 1)
	args := rewritten[0].ToolCalls[0].Function.Arguments
	assert.NotContains(t, args, secret)
	assert.Contains(t, args, portcullis.Marker)
}

// TestRedactSecretsScrubsToolOutput exercises the
// tool_response_transform leg: the same builtin scrubs the tool's
// textual response and returns the rewrite via
// HookSpecificOutput.UpdatedToolResponse. The pointer-typed return
// is honoured by the dispatcher even when the rewrite is the empty
// string — see the field comment.
func TestRedactSecretsScrubsToolOutput(t *testing.T) {
	t.Parallel()

	secret := fakeGitHubPAT()
	in := &hooks.Input{
		HookEventName: hooks.EventToolResponseTransform,
		ToolName:      "shell",
		ToolResponse:  "output: " + secret + " please use it",
	}

	out, err := redactSecrets(t.Context(), in, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput)
	assert.Equal(t, hooks.EventToolResponseTransform, out.HookSpecificOutput.HookEventName)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse,
		"a hit must yield a non-nil pointer (nil signals 'no rewrite')")
	assert.NotContains(t, *out.HookSpecificOutput.UpdatedToolResponse, secret)
	assert.Contains(t, *out.HookSpecificOutput.UpdatedToolResponse, portcullis.Marker)
}

// TestRedactSecretsToolOutputNoOpOnCleanResponse: clean output ⇒ nil
// Output ⇒ dispatcher takes the no-rewrite fast path. Pin this so
// future refactors don't accidentally start rewriting every response
// to the same string and bloating event traffic.
func TestRedactSecretsToolOutputNoOpOnCleanResponse(t *testing.T) {
	t.Parallel()

	out, err := redactSecrets(t.Context(), &hooks.Input{
		HookEventName: hooks.EventToolResponseTransform,
		ToolName:      "shell",
		ToolResponse:  "clean output, nothing to scrub",
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

// TestRedactSecretsToolOutputIgnoresNonStringResponse: structured MCP
// responses (anything other than a plain string) are out of scope for
// this scrubber — the dispatcher only feeds strings to the LLM, so
// rewriting structured shapes would be both a wire-protocol break and
// dead code. Returning nil keeps the runtime's contract intact.
func TestRedactSecretsToolOutputIgnoresNonStringResponse(t *testing.T) {
	t.Parallel()

	out, err := redactSecrets(t.Context(), &hooks.Input{
		HookEventName: hooks.EventToolResponseTransform,
		ToolName:      "mcp_thing",
		ToolResponse:  map[string]any{"structured": "payload"},
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

// TestRedactSecretsLenientOnUnsupportedEvent: configuring the builtin
// under an event that doesn't know how to handle it (e.g. session_start)
// must be a no-op rather than crash the run loop. The event-keyed
// dispatch is the safety net behind the agent flag's auto-injection
// AND user-authored YAML — a typo here used to mean "redact_secrets
// silently does nothing under the wrong event"; with the lenient
// no-op + log we keep the behaviour but make the misconfiguration
// observable.
func TestRedactSecretsLenientOnUnsupportedEvent(t *testing.T) {
	t.Parallel()

	out, err := redactSecrets(t.Context(), &hooks.Input{
		HookEventName: hooks.EventSessionStart,
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

// TestRedactSecretsToolInputTransformEndToEnd wires the auto-injected
// config through a real executor: dispatching tool_input_transform
// yields the scrubbed arguments in Result.ModifiedInput, and nothing
// runs on pre_tool_use anymore.
func TestRedactSecretsToolInputTransformEndToEnd(t *testing.T) {
	t.Parallel()

	reg := hooks.NewRegistry()
	require.NoError(t, Register(reg))
	cfg := ApplyAgentDefaults(nil, AgentDefaults{RedactSecrets: true})
	exec := hooks.NewExecutorWithRegistry(cfg, t.TempDir(), nil, reg)
	assert.False(t, exec.Has(hooks.EventPreToolUse))
	require.True(t, exec.Has(hooks.EventToolInputTransform))

	secret := fakeGitHubPAT()
	result, err := exec.Dispatch(t.Context(), hooks.EventToolInputTransform, &hooks.Input{
		ToolName:  "shell",
		ToolInput: map[string]any{"cmd": "echo " + secret, "cwd": "/tmp"},
	})
	require.NoError(t, err)
	assert.True(t, result.Allowed)
	require.NotNil(t, result.ModifiedInput)
	cmd, _ := result.ModifiedInput["cmd"].(string)
	assert.NotContains(t, cmd, secret)
	assert.Contains(t, cmd, portcullis.Marker)
	assert.Equal(t, "/tmp", result.ModifiedInput["cwd"], "untouched keys are preserved")
}

// requestContextWith mirrors the layout of Anthropic's persisted
// conversation context: a baseline system block plus an update whose
// turn-scoped text is replayed verbatim on every later request.
func requestContextWith(transient string) json.RawMessage {
	return json.RawMessage(`{"v":1,"baseline":{"system":[{"type":"text","text":"You are helpful"}],"tools":[{"name":"shell","input_schema":{"type":"object","maximum":9007199254740993}}]},"update":{"add_tools":["shell"],"transient":["` + transient + `"]}}`)
}

// TestRedactSecretsBeforeLLMCallScrubsProviderRequestContext: a secret
// that only survives in ProviderState.RequestContext (the visible fields
// are clean) is still a rewrite. The signed raw Content is left alone
// and the caller's message keeps its plaintext state.
func TestRedactSecretsBeforeLLMCallScrubsProviderRequestContext(t *testing.T) {
	t.Parallel()

	secret := fakeGitHubPAT()
	rawContent := json.RawMessage(`[{"type":"text","text":"ok","signature":"` + secret + `"}]`)
	original := chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "ok",
		ProviderState: &chat.ProviderState{
			Provider:       "anthropic",
			Content:        rawContent,
			ContentHash:    "h",
			RequestContext: requestContextWith("the token is " + secret),
		},
	}
	in := &hooks.Input{HookEventName: hooks.EventBeforeLLMCall, Messages: []chat.Message{original}}

	out, err := redactSecrets(t.Context(), in, nil)
	require.NoError(t, err)
	require.NotNil(t, out, "a request-context hit must produce a rewrite")
	rewritten := out.HookSpecificOutput.UpdatedMessages
	require.Len(t, rewritten, 1)

	state := rewritten[0].ProviderState
	require.NotNil(t, state)
	assert.NotContains(t, string(state.RequestContext), secret)
	assert.Contains(t, string(state.RequestContext), portcullis.Marker)
	assert.Equal(t, "anthropic", state.Provider)
	assert.Equal(t, "h", state.ContentHash)
	assert.Equal(t, rawContent, state.Content, "signed raw content must be left untouched")

	var cc struct {
		Version  int `json:"v"`
		Baseline struct {
			System []struct{ Text string } `json:"system"`
			Tools  []json.RawMessage       `json:"tools"`
		} `json:"baseline"`
		Update struct {
			AddTools  []string `json:"add_tools"`
			Transient []string `json:"transient"`
		} `json:"update"`
	}
	require.NoError(t, json.Unmarshal(state.RequestContext, &cc))
	assert.Equal(t, 1, cc.Version)
	assert.Equal(t, "You are helpful", cc.Baseline.System[0].Text)
	assert.Equal(t, []string{"shell"}, cc.Update.AddTools)
	require.Len(t, cc.Update.Transient, 1)
	assert.Equal(t, "the token is "+portcullis.Marker, cc.Update.Transient[0])
	assert.Contains(t, string(cc.Baseline.Tools[0]), "9007199254740993", "numbers must survive the rewrite exactly")

	assert.Contains(t, string(in.Messages[0].ProviderState.RequestContext), secret,
		"the stored session message must not be mutated")
	assert.NotSame(t, in.Messages[0].ProviderState, rewritten[0].ProviderState)
}

// TestRedactSecretsBeforeLLMCallScrubsCompactionRequestContext: the
// compaction summary carries its own request baseline; it is scrubbed
// while the opaque block and the summary text stay as they were.
func TestRedactSecretsBeforeLLMCallScrubsCompactionRequestContext(t *testing.T) {
	t.Parallel()

	secret := fakeGitHubPAT()
	block := json.RawMessage(`{"type":"compaction","content":"opaque"}`)
	original := chat.Message{
		Role:    chat.MessageRoleUser,
		Content: chat.SummaryMessageContent("summary"),
		Compaction: &chat.CompactionResult{
			Summary:        "summary",
			Block:          block,
			Provider:       "anthropic",
			RequestContext: requestContextWith(secret),
		},
	}
	in := &hooks.Input{HookEventName: hooks.EventBeforeLLMCall, Messages: []chat.Message{original}}

	out, err := redactSecrets(t.Context(), in, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	rewritten := out.HookSpecificOutput.UpdatedMessages
	require.Len(t, rewritten, 1)

	c := rewritten[0].Compaction
	require.NotNil(t, c)
	assert.NotContains(t, string(c.RequestContext), secret)
	assert.Contains(t, string(c.RequestContext), portcullis.Marker)
	assert.Equal(t, block, c.Block)
	assert.Equal(t, "summary", c.Summary)
	assert.Equal(t, original.Content, rewritten[0].Content)
	assert.Contains(t, string(in.Messages[0].Compaction.RequestContext), secret,
		"the stored session message must not be mutated")
}

// TestRedactSecretsBeforeLLMCallKeepsCleanRequestContextBytes: a clean
// request log is neither rewritten nor re-encoded, and malformed bytes
// pass through so the provider keeps ignoring them.
func TestRedactSecretsBeforeLLMCallKeepsCleanRequestContextBytes(t *testing.T) {
	t.Parallel()

	for name, raw := range map[string]json.RawMessage{
		"clean":     requestContextWith("remember to be brief"),
		"malformed": json.RawMessage(`{"v":1,`),
		"empty":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			in := &hooks.Input{
				HookEventName: hooks.EventBeforeLLMCall,
				Messages: []chat.Message{{
					Role:          chat.MessageRoleAssistant,
					Content:       "ok",
					ProviderState: &chat.ProviderState{Provider: "anthropic", RequestContext: raw},
				}},
			}

			out, err := redactSecrets(t.Context(), in, nil)
			require.NoError(t, err)
			assert.Nil(t, out, "clean request context ⇒ nil Output")
		})
	}
}

func TestRedactSecretsBeforeLLMCallScrubsDocumentsWithoutMutatingHistory(t *testing.T) {
	t.Parallel()
	secret := fakeGitHubPAT()
	for _, field := range []string{"text", "name", "mime"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			doc := &chat.Document{Name: "notes.json", MimeType: "application/json", Source: chat.DocumentSource{InlineText: "clean"}, Size: 5}
			switch field {
			case "text":
				doc.Source.InlineText = `{"token":"` + secret + `"}`
				doc.Size = int64(len(doc.Source.InlineText))
			case "name":
				doc.Name = secret
			case "mime":
				doc.MimeType = "text/plain; token=" + secret
			}
			original := *doc
			in := &hooks.Input{HookEventName: hooks.EventBeforeLLMCall, Messages: []chat.Message{{
				Role:         chat.MessageRoleUser,
				MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeDocument, Document: doc}},
			}}}
			out, err := redactSecrets(t.Context(), in, nil)
			require.NoError(t, err)
			require.NotNil(t, out, "document-only changes must trigger UpdatedMessages")
			updated := out.HookSpecificOutput.UpdatedMessages[0].MultiContent[0].Document
			require.NotSame(t, doc, updated)
			assert.NotContains(t, updated.Source.InlineText, secret)
			assert.NotContains(t, updated.Name, secret)
			assert.NotContains(t, updated.MimeType, secret)
			assert.Equal(t, int64(len(updated.Source.InlineText)), updated.Size)
			assert.Equal(t, original, *doc)
		})
	}
}

func TestRedactSecretsCleanAndBinaryDocuments(t *testing.T) {
	t.Parallel()
	in := &hooks.Input{HookEventName: hooks.EventBeforeLLMCall, Messages: []chat.Message{{MultiContent: []chat.MessagePart{
		{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "notes", MimeType: "text/plain", Source: chat.DocumentSource{InlineText: "clean"}, Size: 5}},
		{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "image", MimeType: "image/png", Source: chat.DocumentSource{InlineData: []byte("opaque bytes")}}},
		{Type: chat.MessagePartTypeDocument},
	}}}}
	out, err := redactSecrets(t.Context(), in, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}
