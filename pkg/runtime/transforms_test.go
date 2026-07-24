// TestApplyBeforeLLMCallTransforms_NoTransformsIsCheap covers the hot
package runtime

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// modalityModelStore returns a fixed [modelsdev.Model] regardless of
// the requested ID. Runstream-level tests configure its Modalities to
// exercise the loop's capability resolution feeding the
// strip_unsupported_modalities transform.
type modalityModelStore struct {
	ModelStore

	model *modelsdev.Model
	err   error
}

func (m modalityModelStore) GetModel(_ context.Context, _ modelsdev.ID) (*modelsdev.Model, error) {
	return m.model, m.err
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

// mixedMediaMsg is a user message carrying every strippable media kind
// (legacy image URL, audio document, video document) plus text and a
// PDF document that the transform must never touch.
func mixedMediaMsg() chat.Message {
	return chat.Message{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "look at this"},
			{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "data:image/png;base64,abc"}},
			{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "clip.wav", MimeType: "audio/wav", Source: chat.DocumentSource{InlineData: []byte{0x52}}}},
			{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "clip.mp4", MimeType: "video/mp4", Source: chat.DocumentSource{InlineData: []byte{0x00}}}},
			{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "report.pdf", MimeType: "application/pdf", Source: chat.DocumentSource{InlineData: []byte{0x25}}}},
			{Type: chat.MessagePartTypeText, Text: "and this"},
		},
	}
}

// TestStripUnsupportedModalitiesTransform pins the capability matrix of
// the runtime-shipped transform. The transform consumes the resolved
// capability set from [hooks.Input.ModelCapabilities]; it never queries
// models.dev itself (the runtime is built with a deliberately
// contradictory multimodal store to prove that).
func TestStripUnsupportedModalitiesTransform(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/model", stream: &mockStream{}}
	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))

	capsPtr := func(image, pdf, audio, video bool) *modelinfo.ModelCapabilities {
		mc := modelinfo.CapsWith(image, pdf, audio, video)
		return &mc
	}

	cases := []struct {
		name string
		caps *modelinfo.ModelCapabilities
		// wantMime lists the expected surviving MultiContent parts, in
		// order, identified by text or MIME/kind.
		wantParts []string
	}{
		{
			name:      "all media capabilities on retains everything",
			caps:      capsPtr(true, true, true, true),
			wantParts: []string{"text", "image", "audio", "video", "pdf", "text"},
		},
		{
			name:      "text-only strips image, audio, and video but keeps text and pdf",
			caps:      capsPtr(false, false, false, false),
			wantParts: []string{"text", "pdf", "text"},
		},
		{
			name:      "audio-only override keeps audio, strips image and video",
			caps:      capsPtr(false, false, true, false),
			wantParts: []string{"text", "audio", "pdf", "text"},
		},
		{
			name:      "unknown model resolves to conservative caps upstream and strips",
			caps:      &modelinfo.ModelCapabilities{},
			wantParts: []string{"text", "pdf", "text"},
		},
		{
			name:      "nil capabilities pass messages through untouched",
			caps:      nil,
			wantParts: []string{"text", "image", "audio", "video", "pdf", "text"},
		},
	}

	// The store contradicts every restrictive case: if the transform
	// consulted models.dev instead of the resolved caps, nothing would
	// ever be stripped.
	store := modalityModelStore{model: &modelsdev.Model{
		Modalities: modelsdev.Modalities{Input: []string{"text", "image", "audio", "video", "pdf"}},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := NewLocalRuntime(t.Context(), tm, WithModelStore(store))
			require.NoError(t, err)

			got, err := r.stripUnsupportedModalitiesTransform(t.Context(),
				&hooks.Input{ModelID: "test/model", ModelCapabilities: tc.caps},
				[]chat.Message{mixedMediaMsg()})
			require.NoError(t, err)
			require.Len(t, got, 1)

			var kinds []string
			for _, p := range got[0].MultiContent {
				switch {
				case p.Type == chat.MessagePartTypeText:
					kinds = append(kinds, "text")
				case p.Type == chat.MessagePartTypeImageURL:
					kinds = append(kinds, "image")
				case p.Document != nil && p.Document.MimeType == "application/pdf":
					kinds = append(kinds, "pdf")
				case p.Document != nil:
					kinds = append(kinds, strings.SplitN(p.Document.MimeType, "/", 2)[0])
				}
			}
			assert.Equal(t, tc.wantParts, kinds, "surviving parts (and their order) must match")
		})
	}
}

// TestStripUnsupportedModalitiesTransform_EmitsDebugLog verifies each
// stripped part is reported at Debug level with its media kind and a
// reason, so operators can trace why content never reached the model.
//
// It swaps the default slog logger and is deliberately NOT parallel so
// no other test logs into the buffer concurrently.
func TestStripUnsupportedModalitiesTransform_EmitsDebugLog(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	prov := &mockProvider{id: "test/model", stream: &mockStream{}}
	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))
	r, err := NewLocalRuntime(t.Context(), tm, WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	textOnly := modelinfo.CapsWith(false, false, false, false)
	_, err = r.stripUnsupportedModalitiesTransform(t.Context(),
		&hooks.Input{ModelID: "test/model", ModelCapabilities: &textOnly},
		[]chat.Message{mixedMediaMsg()})
	require.NoError(t, err)

	logged := buf.String()
	for _, kind := range []string{"image", "audio", "video"} {
		assert.Contains(t, logged, "kind="+kind, "stripped %s part must be logged with its kind", kind)
		assert.Contains(t, logged, "model does not support "+kind+" input",
			"stripped %s part must be logged with a reason", kind)
	}
}

// path: a runtime with no registered transforms returns the input
// slice as-is without allocating a [hooks.Input].
func TestApplyBeforeLLMCallTransforms_NoTransformsIsCheap(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))
	r, err := NewLocalRuntime(t.Context(), tm, WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// Drop the runtime-shipped strip transform so we can observe the
	// cheap-path behavior.
	r.transforms = nil

	sess := session.New(session.WithUserMessage("hi"))
	msgs := []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}

	got := r.applyBeforeLLMCallTransforms(t.Context(), sess, a, "", nil, msgs)
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
	r, err := NewLocalRuntime(t.Context(), tm,
		WithModelStore(mockModelStore{}),
		WithMessageTransform("tag_a", tag("tag_a")),
		WithMessageTransform("tag_b", tag("tag_b")),
	)
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("hi"))
	got := r.applyBeforeLLMCallTransforms(t.Context(), sess, a, "test/mock-model", nil,
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
	r, err := NewLocalRuntime(t.Context(), tm,
		WithModelStore(mockModelStore{}),
		WithMessageTransform("failing", failing),
		WithMessageTransform("tag", tag),
	)
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("hi"))
	got := r.applyBeforeLLMCallTransforms(t.Context(), sess, a, "test/mock-model", nil,
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
	r, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(store))
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

// capsOverrideProvider is a recording provider whose BaseConfig declares
// an explicit `capabilities:` override, mimicking a model config with a
// capabilities block.
type capsOverrideProvider struct {
	recordingMsgProvider

	caps *latest.CapabilitiesConfig
}

func (p *capsOverrideProvider) BaseConfig() base.Config {
	return base.Config{ModelConfig: latest.ModelConfig{Capabilities: p.caps}}
}

// TestRunStream_CapabilityOverrideWinsOverModelsDev is the end-to-end
// regression test for the Step 3 override contract: a model that
// models.dev catalogues as text-only but whose config declares
// `capabilities.image: true` must NOT have its images stripped — the
// loop resolves capabilities with the override applied and the
// transform consumes that result instead of querying models.dev.
func TestRunStream_CapabilityOverrideWinsOverModelsDev(t *testing.T) {
	t.Parallel()

	stream := newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()
	prov := &capsOverrideProvider{
		recordingMsgProvider: recordingMsgProvider{mockProvider: mockProvider{id: "custom/vision", stream: stream}},
		caps:                 &latest.CapabilitiesConfig{Image: true},
	}

	a := agent.New("root", "instructions", agent.WithModel(prov))
	tm := team.New(team.WithAgents(a))

	// models.dev claims text-only — without the override the image would
	// be stripped (see TestRunStream_StripsImagesForTextOnlyModel).
	store := modalityModelStore{model: &modelsdev.Model{
		Modalities: modelsdev.Modalities{Input: []string{"text"}},
	}}
	r, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(store))
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
	var sawImage bool
	for _, m := range prov.got[0] {
		for _, p := range m.MultiContent {
			if p.Type == chat.MessagePartTypeImageURL {
				sawImage = true
			}
		}
	}
	assert.True(t, sawImage, "explicit capabilities.image override must keep images despite a text-only models.dev record")
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
	r, err := NewLocalRuntime(t.Context(), tm,
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

	r, err := NewLocalRuntime(t.Context(), tm,
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
