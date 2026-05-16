// TestApplyBeforeLLMCallTransforms_NoTransformsIsCheap covers the hot
package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// modalityModelStore returns a fixed [modelsdev.Model] regardless of
// the requested ID. Tests configure its Modalities to exercise the
// strip_unsupported_modalities transform's three branches: text-only
// (strip), image-supporting (no-op), and unknown-model (no-op).
type modalityModelStore struct {
	ModelStore

	model *modelsdev.Model
	err   error
}

func (m modalityModelStore) GetModel(_ context.Context, _ modelsdev.ID) (*modelsdev.Model, error) {
	return m.model, m.err
}

// modalityByIDStore returns a different [modelsdev.Model] depending
// on the requested ID, letting tests prove the transform consulted
// the right ID (via [hooks.Input.ModelID]) rather than recomputing
// it from the agent.
type modalityByIDStore struct {
	ModelStore

	models map[string]*modelsdev.Model
}

func (m modalityByIDStore) GetModel(_ context.Context, id modelsdev.ID) (*modelsdev.Model, error) {
	return m.models[id.String()], nil
}

// recordingMsgProvider captures the messages each model call sees so
// a test can confirm a transform actually rewrote what reached the
// provider (rather than just what the in-memory slice ended up
// looking like).
type recordingMsgProvider struct {
	mockProvider

	got [][]chat.Message
}

func (p *recordingMsgProvider) CreateChatCompletionStream(_ context.Context, msgs []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.got = append(p.got, append([]chat.Message{}, msgs...))
	return p.stream, nil
}

// TestStripUnsupportedModalitiesTransform pins the three branches of
// the runtime-shipped transform: a text-only model strips images, a
// multimodal model passes them through, and an unknown model also
// passes them through (the call surfaces any modality mismatch as a
// provider error rather than panicking transform-side).
func TestStripUnsupportedModalitiesTransform(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/model", stream: &mockStream{}}
	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))

	imgMsg := chat.Message{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "look at this"},
			{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "data:image/png;base64,abc"}},
		},
	}

	cases := []struct {
		name      string
		store     modalityModelStore
		modelID   string
		wantStrip bool
	}{
		{name: "text-only model strips images", modelID: "test/text", store: modalityModelStore{model: &modelsdev.Model{Modalities: modelsdev.Modalities{Input: []string{"text"}}}}, wantStrip: true},
		{name: "multimodal model passes through", modelID: "test/multimodal", store: modalityModelStore{model: &modelsdev.Model{Modalities: modelsdev.Modalities{Input: []string{"text", "image"}}}}},
		{name: "nil model passes through", modelID: "test/unknown", store: modalityModelStore{model: nil}},
		{name: "lookup error passes through", modelID: "test/unknown", store: modalityModelStore{err: errors.New("not found")}},
		{name: "empty modalities passes through", modelID: "test/empty", store: modalityModelStore{model: &modelsdev.Model{}}},
		{name: "empty ModelID passes through", modelID: "", store: modalityModelStore{model: &modelsdev.Model{Modalities: modelsdev.Modalities{Input: []string{"text"}}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewLocalRuntime(tm, WithModelStore(tc.store))
			require.NoError(t, err)

			got, err := r.stripUnsupportedModalitiesTransform(t.Context(),
				&hooks.Input{ModelID: tc.modelID}, []chat.Message{imgMsg})
			require.NoError(t, err)
			require.Len(t, got, 1)
			if tc.wantStrip {
				require.Len(t, got[0].MultiContent, 1, "image part must be stripped")
				assert.Equal(t, chat.MessagePartTypeText, got[0].MultiContent[0].Type)
			} else {
				assert.Equal(t, imgMsg, got[0], "messages must reach the model untouched")
			}
		})
	}
}

// TestStripUnsupportedModalitiesTransform_UsesInputModelID pins the
// fix for an alloy-mode / per-tool-override correctness bug: the
// transform must trust [hooks.Input.ModelID] (populated by the loop
// with the model it actually picked) and NOT recompute the model by
// calling agent.Model() — doing so would re-randomize the alloy
// pick and miss any per-tool override the loop had applied.
//
// The test wires a store that reports text-only for one ID and
// multimodal for another. Querying by the text-only ID must strip;
// querying by the multimodal ID must pass through. The agent's own
// model (its pool) is irrelevant — it's never consulted.
func TestStripUnsupportedModalitiesTransform_UsesInputModelID(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/agent-pool-model", stream: &mockStream{}}
	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))

	store := modalityByIDStore{models: map[string]*modelsdev.Model{
		"text/only":             {Modalities: modelsdev.Modalities{Input: []string{"text"}}},
		"multi/modal":           {Modalities: modelsdev.Modalities{Input: []string{"text", "image"}}},
		"test/agent-pool-model": {Modalities: modelsdev.Modalities{Input: []string{"text", "image"}}},
	}}
	r, err := NewLocalRuntime(tm, WithModelStore(store))
	require.NoError(t, err)

	imgMsg := chat.Message{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "describe"},
			{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "data:image/png;base64,abc"}},
		},
	}

	// ModelID = text-only — strip must happen even though the agent's
	// pool model is multimodal.
	stripped, err := r.stripUnsupportedModalitiesTransform(t.Context(),
		&hooks.Input{ModelID: "text/only"}, []chat.Message{imgMsg})
	require.NoError(t, err)
	require.Len(t, stripped[0].MultiContent, 1, "image must be stripped when ModelID is text-only")
	assert.Equal(t, chat.MessagePartTypeText, stripped[0].MultiContent[0].Type)

	// ModelID = multimodal — strip must NOT happen even if some other
	// model in scope is text-only. Proves the lookup keys off ModelID.
	passed, err := r.stripUnsupportedModalitiesTransform(t.Context(),
		&hooks.Input{ModelID: "multi/modal"}, []chat.Message{imgMsg})
	require.NoError(t, err)
	assert.Equal(t, imgMsg, passed[0], "images must reach a multimodal ModelID untouched")
}

// path: a runtime with no registered transforms returns the input
// slice as-is without allocating a [hooks.Input].
func TestApplyBeforeLLMCallTransforms_NoTransformsIsCheap(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))
	r, err := NewLocalRuntime(tm, WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// Drop the runtime-shipped strip transform so we can observe the
	// cheap-path behavior.
	r.transforms = nil

	sess := session.New(session.WithUserMessage("hi"))
	msgs := []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}

	got := r.applyBeforeLLMCallTransforms(t.Context(), sess, a, "", msgs)
	assert.Equal(t, msgs, got)
}

// TestApplyBeforeLLMCallTransforms_OrderAndChain verifies that
// transforms registered via [WithMessageTransform] run in
// registration order and feed each transform the cumulative output of
// the previous one (chain semantics, not parallel).
func TestApplyBeforeLLMCallTransforms_OrderAndChain(t *testing.T) {
	t.Parallel()

	type call struct {
		name   string
		seenIn int
	}
	var calls []call
	tag := func(name string) MessageTransform {
		return func(_ context.Context, _ *hooks.Input, msgs []chat.Message) ([]chat.Message, error) {
			calls = append(calls, call{name: name, seenIn: len(msgs)})
			return append(msgs, chat.Message{Role: chat.MessageRoleSystem, Content: name}), nil
		}
	}

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))
	r, err := NewLocalRuntime(tm,
		WithModelStore(mockModelStore{}),
		WithMessageTransform("tag_a", tag("tag_a")),
		WithMessageTransform("tag_b", tag("tag_b")),
	)
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("hi"))
	got := r.applyBeforeLLMCallTransforms(t.Context(), sess, a, "test/mock-model",
		[]chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}})

	require.Len(t, calls, 2, "expected tag_a + tag_b to fire exactly once each")
	assert.Equal(t, "tag_a", calls[0].name, "transforms must run in registration order")
	assert.Equal(t, "tag_b", calls[1].name)
	assert.Greater(t, calls[1].seenIn, calls[0].seenIn,
		"tag_b must see tag_a's appended message (chain semantics, not parallel)")

	var contents []string
	for _, m := range got {
		contents = append(contents, m.Content)
	}
	assert.Contains(t, contents, "tag_a")
	assert.Contains(t, contents, "tag_b")
}

// TestApplyBeforeLLMCallTransforms_ErrorsAreSwallowed pins the
// fail-soft contract: a transform that returns an error must NOT
// break the run loop; the previous slice continues through the
// chain.
func TestApplyBeforeLLMCallTransforms_ErrorsAreSwallowed(t *testing.T) {
	t.Parallel()

	failing := func(_ context.Context, _ *hooks.Input, _ []chat.Message) ([]chat.Message, error) {
		return nil, errors.New("boom")
	}
	tag := func(_ context.Context, _ *hooks.Input, msgs []chat.Message) ([]chat.Message, error) {
		return append(msgs, chat.Message{Role: chat.MessageRoleSystem, Content: "after_failure"}), nil
	}

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))
	r, err := NewLocalRuntime(tm,
		WithModelStore(mockModelStore{}),
		WithMessageTransform("failing", failing),
		WithMessageTransform("tag", tag),
	)
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("hi"))
	got := r.applyBeforeLLMCallTransforms(t.Context(), sess, a, "test/mock-model",
		[]chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}})

	var contents []string
	for _, m := range got {
		contents = append(contents, m.Content)
	}
	assert.Contains(t, contents, "after_failure",
		"a transform error must not abort the chain")
}

// TestRunStream_StripsImagesForTextOnlyModel is the end-to-end smoke
// test confirming the inline strip in runStreamLoop has been
// replaced: messages reaching the provider must no longer carry
// image parts when the agent's model is text-only.
func TestRunStream_StripsImagesForTextOnlyModel(t *testing.T) {
	t.Parallel()

	stream := newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()
	prov := &recordingMsgProvider{mockProvider: mockProvider{id: "test/text-only", stream: stream}}

	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))

	store := modalityModelStore{model: &modelsdev.Model{
		Modalities: modelsdev.Modalities{Input: []string{"text"}},
	}}
	r, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(store))
	require.NoError(t, err)

	sess := session.New()
	sess.AddMessage(session.UserMessage("",
		chat.MessagePart{Type: chat.MessagePartTypeText, Text: "describe"},
		chat.MessagePart{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "data:image/png;base64,abc"}},
	))

	for range r.RunStream(t.Context(), sess) {
		// drain — only the recorded provider state matters
	}

	require.NotEmpty(t, prov.got, "provider must have been called")
	for _, m := range prov.got[0] {
		for _, p := range m.MultiContent {
			assert.NotEqual(t, chat.MessagePartTypeImageURL, p.Type,
				"image parts must be stripped before reaching a text-only model")
		}
	}
}

// TestRunStream_TransformErrorDoesNotBreakRun is the end-to-end smoke
// test confirming the fail-soft contract: a transform error must not
// prevent the model from being called and the run from completing.
func TestRunStream_TransformErrorDoesNotBreakRun(t *testing.T) {
	t.Parallel()

	stream := newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()
	prov := &mockProvider{id: "test/mock-model", stream: stream}

	failing := func(_ context.Context, _ *hooks.Input, _ []chat.Message) ([]chat.Message, error) {
		return nil, errors.New("boom")
	}

	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))
	r, err := NewLocalRuntime(tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
		WithMessageTransform("failing", failing),
	)
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("hi"))
	var sawStop bool
	for ev := range r.RunStream(t.Context(), sess) {
		if _, ok := ev.(*StreamStoppedEvent); ok {
			sawStop = true
		}
	}
	assert.True(t, sawStop, "run must complete despite a failing transform")
}

// TestWithMessageTransform_RejectsEmptyAndNil pins the input
// validation: empty name or nil fn must be silently ignored
// (matching the no-error shape of other Opts).
func TestWithMessageTransform_RejectsEmptyAndNil(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))

	r, err := NewLocalRuntime(tm,
		WithModelStore(mockModelStore{}),
		WithMessageTransform("", func(_ context.Context, _ *hooks.Input, msgs []chat.Message) ([]chat.Message, error) {
			return msgs, nil
		}),
		WithMessageTransform("nilfn", nil),
	)
	require.NoError(t, err, "WithMessageTransform must not surface a constructor error")

	// Only the runtime-shipped strip_unsupported_modalities transform
	// remains — invalid user transforms are dropped silently. The
	// redact_secrets transform that used to ride alongside has migrated
	// to the hook protocol (pkg/hooks/builtins/redact_secrets.go) so it
	// no longer appears in the message-transform chain.
	require.Len(t, r.transforms, 1, "invalid transforms must be silently ignored")
	assert.Equal(t, BuiltinStripUnsupportedModalities, r.transforms[0].name)
}
