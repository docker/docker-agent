// TestApplyBeforeLLMCallTransforms_NoTransformsIsCheap covers the hot
package runtime

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/paths"
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

// TestStripUnsupportedMediaContent_PreservesGeneratedMediaIndependently is
// the review's "unsupported-modality transform independence" regression:
// stripUnsupportedMediaContent (the shared helper stripUnsupportedModalitiesTransform
// calls) must never strip a generated-media document part on its own,
// even when invoked directly with a capability set that would otherwise
// reject its MIME kind and even though [BuiltinStripGeneratedMedia] never
// ran first. This is deliberately independent of transform REGISTRATION
// ORDER: it calls the low-level helper directly rather than going through
// RunStream/NewLocalRuntime, so a future reordering of runtime.go's
// transform chain cannot silently reintroduce the bug this guards against
// (see isGeneratedMediaPart's use inside stripUnsupportedMediaContent).
// The production-order integration coverage in
// TestRunStream_MediaOnlyAssistantHistoryRemainsCoherent_UnknownModel stays
// in place alongside this test, not replaced by it.
//
// Both an owner-qualified marker (ArtifactOwnerSessionID set, the shape
// every current write path produces) and a legacy ownerless marker
// (ArtifactOwnerSessionID empty, the shape a message persisted before
// owner-qualified references existed would still carry) are covered: the
// guard is [isGeneratedMediaPart], which keys off ArtifactPath alone, so
// an old, ownerless marker must survive identically rather than being
// silently treated as an ordinary attachment now that it lacks an owner.
func TestStripUnsupportedMediaContent_PreservesGeneratedMediaIndependently(t *testing.T) {
	t.Parallel()

	// Text-only: image support is off, so an ordinary (non-generated) image
	// part would normally be stripped by this exact call.
	mc := modelinfo.CapsWith(false, false, false, false)

	ownerQualified := chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
		Name: "cat.png", MimeType: "image/png",
		Source: chat.DocumentSource{ArtifactPath: "cat.png", ArtifactOwnerSessionID: "sess-1"},
	}}
	legacyOwnerless := chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
		Name: "dog.jpg", MimeType: "image/jpeg",
		Source: chat.DocumentSource{ArtifactPath: "dog.jpg"},
	}}

	msgs := []chat.Message{
		{
			Role:    chat.MessageRoleAssistant,
			Content: "here you go",
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "here you go"},
				ownerQualified,
				legacyOwnerless,
				// An ordinary user-attached image (no ArtifactPath) in the same
				// message must still be stripped, proving the guard is scoped to
				// generated-media parts only, not a blanket image exemption.
				{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "data:image/png;base64,abc"}},
			},
		},
	}

	out := stripUnsupportedMediaContent(t.Context(), msgs, mc)
	require.Len(t, out, 1)

	var sawOwnerQualified, sawLegacyOwnerless, sawImageURL bool
	for _, p := range out[0].MultiContent {
		switch {
		case isGeneratedMediaPart(p) && p.Document.Source.ArtifactOwnerSessionID != "":
			assert.Equal(t, ownerQualified, p, "an owner-qualified generated-media marker must survive byte-identical")
			sawOwnerQualified = true
		case isGeneratedMediaPart(p):
			assert.Equal(t, legacyOwnerless, p, "a legacy ownerless generated-media marker must survive byte-identical")
			sawLegacyOwnerless = true
		}
		if p.Type == chat.MessagePartTypeImageURL {
			sawImageURL = true
		}
	}
	assert.True(t, sawOwnerQualified,
		"an owner-qualified generated-media part must survive stripUnsupportedMediaContent independently of strip_generated_media having run first")
	assert.True(t, sawLegacyOwnerless,
		"a legacy ownerless generated-media part must survive stripUnsupportedMediaContent independently of strip_generated_media having run first")
	assert.False(t, sawImageURL, "an ordinary user-attached image without ArtifactPath must still be stripped")
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

	// Only the runtime-shipped strip_unsupported_modalities and
	// strip_generated_media transforms remain — invalid user transforms
	// are dropped silently. The redact_secrets transform that used to
	// ride alongside has migrated to the hook protocol
	// (pkg/hooks/builtins/redact_secrets.go) so it no longer appears in
	// the message-transform chain.
	require.Len(t, r.transforms, 2, "invalid transforms must be silently ignored")
	assert.Equal(t, BuiltinStripGeneratedMedia, r.transforms[0].name, "strip_generated_media must run before strip_unsupported_modalities so its placeholder logic sees the media part first")
	assert.Equal(t, BuiltinStripUnsupportedModalities, r.transforms[1].name)
}

// TestStripGeneratedMediaTransform verifies the transform's part-level
// selection logic directly: it must remove only assistant document parts
// carrying an ArtifactPath (runtime-materialized, model-generated media),
// leaving user-attached documents (InlineData) and surrounding text intact
// on every role.
func TestStripGeneratedMediaTransform(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{
		session.UserMessage("draw a cat",
			chat.MessagePart{Type: chat.MessagePartTypeText, Text: "draw a cat"},
			chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
				Name: "reference.png", MimeType: "image/png",
				Source: chat.DocumentSource{InlineData: []byte{0x01}},
			}},
		).Message,
		{
			Role:    chat.MessageRoleAssistant,
			Content: "here you go",
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "here you go"},
				{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
					Name: "cat.png", MimeType: "image/png", Size: 4,
					Source: chat.DocumentSource{ArtifactPath: "generated/cat.png"},
				}},
			},
		},
	}

	out, err := stripGeneratedMediaTransform(t.Context(), nil, msgs)
	require.NoError(t, err)
	require.Len(t, out, 2)

	// The user's own attachment (InlineData, not an artifact reference)
	// must never be touched by this transform.
	require.Len(t, out[0].MultiContent, 2)
	assert.Equal(t, chat.MessagePartTypeDocument, out[0].MultiContent[1].Type)
	assert.NotEmpty(t, out[0].MultiContent[1].Document.Source.InlineData)

	// The assistant's generated artifact is stripped; its text survives,
	// and a placeholder part is appended alongside the original text part.
	assert.Equal(t, "here you go\n[Generated media omitted from history 1/1: cat.png (image/png)]", out[1].Content)
	require.Len(t, out[1].MultiContent, 2, "original text part plus one placeholder part for the stripped artifact")
	assert.Equal(t, chat.MessagePartTypeText, out[1].MultiContent[0].Type)
	assert.Equal(t, "here you go", out[1].MultiContent[0].Text, "the original text part must be preserved verbatim")
	assert.Equal(t, chat.MessagePartTypeText, out[1].MultiContent[1].Type)
	assert.Contains(t, out[1].MultiContent[1].Text, "cat.png")
	assert.Contains(t, out[1].MultiContent[1].Text, "image/png")
}

// TestStripGeneratedMediaTransform_WorkspaceRootReference pins the strip
// predicates for the workspace-materialized reference shape
// (ArtifactRoot=workspace + workspace-relative ArtifactPath): both the
// strip_generated_media placeholder replacement and the
// strip_unsupported_modalities never-resend guard key on a non-empty
// ArtifactPath, so a workspace-rooted part must behave exactly like a
// legacy data-dir one — stripped with a placeholder by the former, never
// silently dropped by the latter.
func TestStripGeneratedMediaTransform_WorkspaceRootReference(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{
		{
			Role:    chat.MessageRoleAssistant,
			Content: "here you go",
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "here you go"},
				{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
					Name: "cat.png", MimeType: "image/png", Size: 4,
					Source: chat.DocumentSource{
						ArtifactPath:           "images/cat.png",
						ArtifactRoot:           chat.ArtifactRootWorkspace,
						ArtifactOwnerSessionID: "owner",
					},
				}},
			},
		},
	}

	out, err := stripGeneratedMediaTransform(t.Context(), nil, msgs)
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Len(t, out[0].MultiContent, 2, "original text part plus one placeholder part")
	assert.Equal(t, chat.MessagePartTypeText, out[0].MultiContent[1].Type)
	assert.Contains(t, out[0].MultiContent[1].Text, "cat.png")
	assert.Equal(t, "here you go\n[Generated media omitted from history 1/1: cat.png (image/png)]", out[0].Content)

	// The modality guard must retain (not silently drop) the workspace-rooted
	// part even for a text-only model — strip_generated_media owns replacing it.
	kept := stripUnsupportedMediaContent(t.Context(), msgs, modelinfo.ModelCapabilities{})
	require.Len(t, kept, 1)
	require.Len(t, kept[0].MultiContent, 2)
	assert.Equal(t, "images/cat.png", kept[0].MultiContent[1].Document.Source.ArtifactPath)
}

// TestStripGeneratedMediaTransform_MultipleArtifacts_MediaOnly is the
// review's "robust multi-artifact placeholder" regression for a media-only
// assistant turn: THREE stripped artifacts in a single message must
// produce exactly one placeholder [chat.MessagePart] PER artifact (never
// one combined blob), each carrying the canonical i/N string in the
// artifacts' original source order, with safe (sanitized) name and MIME
// type metadata.
func TestStripGeneratedMediaTransform_MultipleArtifacts_MediaOnly(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{
		{
			Role:    chat.MessageRoleAssistant,
			Content: "",
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
					Name: "cat.png", MimeType: "image/png",
					Source: chat.DocumentSource{ArtifactPath: "cat.png", ArtifactOwnerSessionID: "sess-multi"},
				}},
				{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
					Name: "dog.jpg", MimeType: "image/jpeg",
					Source: chat.DocumentSource{ArtifactPath: "dog.jpg", ArtifactOwnerSessionID: "sess-multi"},
				}},
				{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
					Name: "fish.gif", MimeType: "image/gif",
					Source: chat.DocumentSource{ArtifactPath: "fish.gif", ArtifactOwnerSessionID: "sess-multi"},
				}},
			},
		},
	}

	out, err := stripGeneratedMediaTransform(t.Context(), nil, msgs)
	require.NoError(t, err)
	require.Len(t, out, 1)

	assistant := out[0]
	require.Len(t, assistant.MultiContent, 3, "one placeholder part per stripped artifact, never a single combined blob")

	want := []string{
		"[Generated media omitted from history 1/3: cat.png (image/png)]",
		"[Generated media omitted from history 2/3: dog.jpg (image/jpeg)]",
		"[Generated media omitted from history 3/3: fish.gif (image/gif)]",
	}
	for i, w := range want {
		assert.Equal(t, chat.MessagePartTypeText, assistant.MultiContent[i].Type)
		assert.Equal(t, w, assistant.MultiContent[i].Text, "placeholder %d must be in the artifacts' original source order", i+1)
	}
	assert.Equal(t, strings.Join(want, "\n"), assistant.Content, "Content must mirror the same per-artifact placeholders, in order, for Content-only readers")
}

// TestStripGeneratedMediaTransform_MultipleArtifacts_Mixed is the mixed
// text+media counterpart: the assistant's original text must survive
// untouched (in both Content and MultiContent) alongside one placeholder
// part per stripped artifact, in source order.
func TestStripGeneratedMediaTransform_MultipleArtifacts_Mixed(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{
		{
			Role:    chat.MessageRoleAssistant,
			Content: "here are three images",
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "here are three images"},
				{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
					Name: "cat.png", MimeType: "image/png",
					Source: chat.DocumentSource{ArtifactPath: "cat.png", ArtifactOwnerSessionID: "sess-multi"},
				}},
				{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
					Name: "dog.jpg", MimeType: "image/jpeg",
					Source: chat.DocumentSource{ArtifactPath: "dog.jpg", ArtifactOwnerSessionID: "sess-multi"},
				}},
				{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
					Name: "fish.gif", MimeType: "image/gif",
					Source: chat.DocumentSource{ArtifactPath: "fish.gif", ArtifactOwnerSessionID: "sess-multi"},
				}},
			},
		},
	}

	out, err := stripGeneratedMediaTransform(t.Context(), nil, msgs)
	require.NoError(t, err)
	require.Len(t, out, 1)

	assistant := out[0]
	require.Len(t, assistant.MultiContent, 4, "original text part plus one placeholder part per stripped artifact")
	assert.Equal(t, chat.MessagePartTypeText, assistant.MultiContent[0].Type)
	assert.Equal(t, "here are three images", assistant.MultiContent[0].Text, "original text must be preserved verbatim")

	want := []string{
		"[Generated media omitted from history 1/3: cat.png (image/png)]",
		"[Generated media omitted from history 2/3: dog.jpg (image/jpeg)]",
		"[Generated media omitted from history 3/3: fish.gif (image/gif)]",
	}
	for i, w := range want {
		assert.Equal(t, chat.MessagePartTypeText, assistant.MultiContent[i+1].Type)
		assert.Equal(t, w, assistant.MultiContent[i+1].Text, "placeholder %d must be in the artifacts' original source order", i+1)
	}
	assert.Equal(t, "here are three images\n"+strings.Join(want, "\n"), assistant.Content,
		"Content must keep the original text then mirror every per-artifact placeholder, in order")
}

// TestStripGeneratedMediaTransform_ResanitizesLegacyUnsafeName is the
// defense-in-depth regression for the plan's "sanitize twice" requirement:
// materializeGeneratedMedia already sanitizes Document.Name before it is
// ever persisted, but a message loaded from an older session (persisted
// before that sanitization existed, or written by some future code path
// that forgets to) could still carry a raw, unsafe name. The placeholder
// must never surface it verbatim.
func TestStripGeneratedMediaTransform_ResanitizesLegacyUnsafeName(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{
		{
			Role: chat.MessageRoleAssistant,
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
					Name: "../../etc/passwd\x00.png", MimeType: "image/png\x01",
					Source: chat.DocumentSource{ArtifactPath: "generated/cat.png"},
				}},
			},
		},
	}

	out, err := stripGeneratedMediaTransform(t.Context(), nil, msgs)
	require.NoError(t, err)
	require.Len(t, out, 1)

	assert.NotContains(t, out[0].Content, "..")
	assert.NotContains(t, out[0].Content, "/etc/passwd")
	assert.NotContains(t, out[0].Content, "\x00")
	assert.NotContains(t, out[0].Content, "\x01")
	require.Len(t, out[0].MultiContent, 1)
	assert.NotContains(t, out[0].MultiContent[0].Text, "..")
	assert.NotContains(t, out[0].MultiContent[0].Text, "/etc/passwd")
}

// TestStripGeneratedMediaTransform_EmptyNameAndMimeFallback is the plan's
// "empty name/MIME placeholder fallback" regression: an empty (or
// all-whitespace) display name and an empty MIME type are both values a
// real provider can legitimately send (e.g. a media delta with no
// InlineData.DisplayName at all). generatedMediaPlaceholderTexts must
// deterministically substitute fallbackDisplayName/fallbackMimeType for
// each rather than ever rendering an empty or malformed "()" in the
// placeholder.
func TestStripGeneratedMediaTransform_EmptyNameAndMimeFallback(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{
		{
			Role: chat.MessageRoleAssistant,
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
					Name: "   ", MimeType: "",
					Source: chat.DocumentSource{ArtifactPath: "generated/blank.bin", ArtifactOwnerSessionID: "sess-1"},
				}},
			},
		},
	}

	out, err := stripGeneratedMediaTransform(t.Context(), nil, msgs)
	require.NoError(t, err)
	require.Len(t, out, 1)

	want := "[Generated media omitted from history 1/1: generated media (application/octet-stream)]"
	assert.Equal(t, want, out[0].Content)
	require.Len(t, out[0].MultiContent, 1)
	assert.Equal(t, want, out[0].MultiContent[0].Text)
}

// assertBoundedSingleLineUTF8 asserts the cross-output invariant every
// final placeholder or [WarningEvent] line must satisfy regardless of how
// overlong or malformed the provider-supplied metadata that fed it was:
// valid UTF-8 (never a truncated multi-byte rune), no control characters
// or newlines (so it can never split into, or masquerade as, an extra
// terminal/log line), and no more than [maxPlaceholderOrWarningBytes] —
// the final backstop applied independently of the smaller
// [chat.MaxSanitizedFieldBytes] bound already enforced on each individual
// field. Shared by the placeholder tests here and the WarningEvent tests
// in materialize_generated_media_test.go so both output kinds are held to
// exactly the same bound.
func assertBoundedSingleLineUTF8(t *testing.T, s string) {
	t.Helper()
	assert.True(t, utf8.ValidString(s), "must be valid UTF-8, never a truncated multi-byte rune")
	assert.LessOrEqual(t, len(s), maxPlaceholderOrWarningBytes, "must never exceed the final formatted-line byte cap")
	for _, r := range s {
		assert.Falsef(t, r < 0x20 || r == 0x7f, "must not contain a control character or newline, got %q in %q", r, s)
	}
}

// TestStripGeneratedMediaTransform_OverlongMetadataStaysBounded is the
// plan's "overlong metadata" regression for the placeholder output: a
// provider-supplied display name and MIME type both well past
// [chat.MaxSanitizedFieldBytes] (128 bytes) — the name built from a
// multi-byte rune so truncation must land on a rune boundary, not merely
// an ASCII one — must still yield a placeholder that is valid UTF-8,
// single-line, control-character-free, and within
// [maxPlaceholderOrWarningBytes] overall, without ever weakening either
// sanitizer's own field bound.
func TestStripGeneratedMediaTransform_OverlongMetadataStaysBounded(t *testing.T) {
	t.Parallel()

	// "é" is 2 UTF-8 bytes; 200 repetitions is 400 bytes, comfortably past
	// the 128-byte field bound, and an odd byte-count truncation point
	// would split the rune if TruncateUTF8Bytes were not rune-boundary safe.
	longName := strings.Repeat("é", 200)
	longMimeType := "image/" + strings.Repeat("x", 300)

	msgs := []chat.Message{
		{
			Role: chat.MessageRoleAssistant,
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
					Name: longName, MimeType: longMimeType,
					Source: chat.DocumentSource{ArtifactPath: "generated/overlong.bin", ArtifactOwnerSessionID: "sess-1"},
				}},
			},
		},
	}

	out, err := stripGeneratedMediaTransform(t.Context(), nil, msgs)
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Len(t, out[0].MultiContent, 1)

	placeholder := out[0].MultiContent[0].Text
	assert.Equal(t, out[0].Content, placeholder, "Content must mirror the MultiContent placeholder exactly")
	assertBoundedSingleLineUTF8(t, placeholder)

	// The raw overlong fields must never survive verbatim: only their
	// sanitized, field-bounded (<=128 bytes) forms may appear.
	assert.NotContains(t, placeholder, longName, "the full 400-byte name must have been truncated, not passed through")
	assert.NotContains(t, placeholder, longMimeType, "the full 306-byte MIME type must have been truncated, not passed through")
	assert.Contains(t, placeholder, "é", "the sanitized (truncated) multi-byte name must still be present")
}

// TestRunStream_GeneratedMediaAbsentFromNextTurnHistory is the end-to-end
// regression test for the "no automatic resend" policy (plan step 4,
// Context bloat decision): a generated image materialized on turn 1 must
// not be replayed to the provider on turn 2, while the assistant's text
// from turn 1 still is.
func TestRunStream_GeneratedMediaAbsentFromNextTurnHistory(t *testing.T) {
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })

	turn1 := newStreamBuilder().
		AddContent("here is your image").
		AddMedia([]byte{0x89, 0x50, 0x4e, 0x47}, "image/png", "cat.png").
		AddStopWithUsage(1, 1).
		Build()
	turn2 := newStreamBuilder().AddContent("sure, noted").AddStopWithUsage(1, 1).Build()

	prov := &recordingMsgProvider{mockProvider: mockProvider{id: "test/mock-model"}}
	queue := []chat.MessageStream{turn1, turn2}
	prov.stream = turn1

	a := agent.New("root", "instructions", agent.WithModel(&queueRecordingProvider{recordingMsgProvider: prov, queue: queue}))
	tm := team.New(team.WithAgents(a))
	r, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("draw a cat"), session.WithWorkingDir(t.TempDir()))
	for range r.RunStream(t.Context(), sess) {
	}

	sess.AddMessage(session.UserMessage("thanks"))
	for range r.RunStream(t.Context(), sess) {
	}

	require.Len(t, prov.got, 2, "the provider must have been called for both turns")

	secondTurnHistory := prov.got[1]
	var assistantMsg *chat.Message
	for i := range secondTurnHistory {
		if secondTurnHistory[i].Role == chat.MessageRoleAssistant {
			assistantMsg = &secondTurnHistory[i]
		}
	}
	require.NotNil(t, assistantMsg, "turn 1's assistant message must be part of turn 2's history")
	assert.Contains(t, assistantMsg.Content, "here is your image", "text must still be sent on the next turn")
	for _, part := range assistantMsg.MultiContent {
		if part.Type == chat.MessagePartTypeDocument && part.Document != nil {
			assert.Empty(t, part.Document.Source.ArtifactPath,
				"generated media must be stripped from outgoing history on the next turn")
		}
	}

	// Sanity-check the artifact really was persisted (not just skipped
	// entirely): the session itself keeps a reference across turns even
	// though it is stripped before reaching the provider.
	var sessionAssistantMsg *chat.Message
	for _, m := range sess.GetAllMessages() {
		if m.Message.Role == chat.MessageRoleAssistant {
			sessionAssistantMsg = &m.Message
			break
		}
	}
	require.NotNil(t, sessionAssistantMsg)
	var foundArtifact bool
	for _, part := range sessionAssistantMsg.MultiContent {
		if part.Type == chat.MessagePartTypeDocument && part.Document != nil && part.Document.Source.ArtifactPath != "" {
			foundArtifact = true
			// The owner must be the session that generated the media, matching
			// the exact session used for materialization (see finding A).
			assert.Equal(t, sess.ID, part.Document.Source.ArtifactOwnerSessionID)
		}
	}
	assert.True(t, foundArtifact, "the session itself must retain the artifact reference")

	// The runtime-produced MultiContent must carry the assistant's text as
	// a text part alongside the document part, not just in .Content —
	// otherwise a provider converter that treats non-empty MultiContent as
	// authoritative (e.g. pkg/model/provider/oaistream) would drop the text
	// entirely whenever this message reaches it un-stripped. This pins the
	// EXACT shape recordAssistantMessage produces (finding D), not a
	// handcrafted fixture.
	require.Len(t, sessionAssistantMsg.MultiContent, 2)
	assert.Equal(t, chat.MessagePartTypeText, sessionAssistantMsg.MultiContent[0].Type)
	assert.Equal(t, "here is your image", sessionAssistantMsg.MultiContent[0].Text)
	assert.Equal(t, chat.MessagePartTypeDocument, sessionAssistantMsg.MultiContent[1].Type)
}

// TestStripGeneratedMediaTransform_MediaOnlyBecomesPlaceholder is the
// regression test for finding D's "no-resend coherence" requirement: a
// media-only assistant message (no text at all) must not be reduced to a
// completely empty message once its generated media is stripped — that
// would either violate providers' payload validity or make the turn
// silently vanish from history (breaking strict user/assistant
// alternation). A stable placeholder keeps the turn present instead.
func TestStripGeneratedMediaTransform_MediaOnlyBecomesPlaceholder(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{
		session.UserMessage("draw a cat").Message,
		{
			Role:    chat.MessageRoleAssistant,
			Content: "",
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
					Name: "cat.png", MimeType: "image/png", Size: 4,
					Source: chat.DocumentSource{ArtifactPath: "cat.png", ArtifactOwnerSessionID: "sess-1"},
				}},
			},
		},
		session.UserMessage("thanks").Message,
	}

	out, err := stripGeneratedMediaTransform(t.Context(), nil, msgs)
	require.NoError(t, err)
	require.Len(t, out, 3, "the media-only assistant turn must remain present, not vanish")

	assistant := out[1]
	assert.Equal(t, chat.MessageRoleAssistant, assistant.Role)
	assert.NotEmpty(t, assistant.Content, "an empty Content plus empty MultiContent would make providers drop the turn")
	assert.Equal(t, "[Generated media omitted from history 1/1: cat.png (image/png)]", assistant.Content)
	require.Len(t, assistant.MultiContent, 1, "media-only history must remain nonempty via a placeholder part, not just Content")
	assert.Equal(t, chat.MessagePartTypeText, assistant.MultiContent[0].Type)
	assert.Equal(t, assistant.Content, assistant.MultiContent[0].Text, "the MultiContent placeholder must mirror Content exactly")

	// The surrounding user turns must be untouched, preserving strict
	// user/assistant alternation end to end.
	assert.Equal(t, chat.MessageRoleUser, out[0].Role)
	assert.Equal(t, chat.MessageRoleUser, out[2].Role)
}

// TestRunStream_MediaOnlyAssistantHistoryRemainsCoherent is the end-to-end
// regression test for finding D: prior user → media-only assistant → next
// user history must remain a valid, alternating conversation once sent to
// the provider, even though the assistant's only content (the generated
// image) is stripped from outgoing history.
func TestRunStream_MediaOnlyAssistantHistoryRemainsCoherent(t *testing.T) {
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })

	turn1 := newStreamBuilder().
		AddMedia([]byte{0x89, 0x50, 0x4e, 0x47}, "image/png", "cat.png").
		AddStopWithUsage(1, 1).
		Build()
	turn2 := newStreamBuilder().AddContent("sure, noted").AddStopWithUsage(1, 1).Build()

	prov := &recordingMsgProvider{mockProvider: mockProvider{id: "test/mock-model"}}
	queue := []chat.MessageStream{turn1, turn2}
	prov.stream = turn1

	a := agent.New("root", "instructions", agent.WithModel(&queueRecordingProvider{recordingMsgProvider: prov, queue: queue}))
	tm := team.New(team.WithAgents(a))
	// The model must support image input, matching a real Gemini
	// image-output model continuing its own turn: this isolates the
	// no-resend policy (strip_generated_media) from the unrelated
	// strip_unsupported_modalities transform, which would otherwise also
	// strip the same part for a capability-less model and mask which
	// transform is actually responsible for the placeholder.
	store := modalityModelStore{model: &modelsdev.Model{
		Modalities: modelsdev.Modalities{Input: []string{"text", "image"}},
	}}
	r, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(store))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("draw a cat"), session.WithWorkingDir(t.TempDir()))
	for range r.RunStream(t.Context(), sess) {
	}

	sess.AddMessage(session.UserMessage("thanks"))
	for range r.RunStream(t.Context(), sess) {
	}

	require.Len(t, prov.got, 2, "the provider must have been called for both turns")

	secondTurnHistory := prov.got[1]
	require.GreaterOrEqual(t, len(secondTurnHistory), 3, "user, assistant, user must all be present")

	// Find the sequence: the media-only assistant turn must sit between
	// the two user turns, not have been dropped.
	var roles []chat.MessageRole
	for _, m := range secondTurnHistory {
		roles = append(roles, m.Role)
	}
	assert.Contains(t, roles, chat.MessageRoleAssistant,
		"the media-only assistant turn must still be present in history, not silently dropped")

	var assistantMsg *chat.Message
	for i := range secondTurnHistory {
		if secondTurnHistory[i].Role == chat.MessageRoleAssistant {
			assistantMsg = &secondTurnHistory[i]
		}
	}
	require.NotNil(t, assistantMsg)
	assert.NotEmpty(t, assistantMsg.Content, "a media-only turn must carry a placeholder, never end up fully empty")
	require.Len(t, assistantMsg.MultiContent, 1, "the generated media itself must still be stripped, replaced by a placeholder part")
	assert.Equal(t, chat.MessagePartTypeText, assistantMsg.MultiContent[0].Type)
	for _, part := range assistantMsg.MultiContent {
		assert.NotEqual(t, chat.MessagePartTypeDocument, part.Type, "no document/media part must survive on the outgoing history")
	}
}

// TestRunStream_MediaOnlyAssistantHistoryRemainsCoherent_UnknownModel is the
// integration regression test for the transform-ordering invariant (Step 4
// remediation, review finding 1): strip_generated_media MUST run before
// strip_unsupported_modalities in the actual production transform chain, so
// a capability-less or unknown model (mockModelStore.GetModel returns a nil
// *modelsdev.Model, which modelinfo.ResolveCapsFromModel turns into the
// conservative text-only default — no image/audio/video support at all)
// never gets a chance to strip a media-only generated-media assistant
// message down to a completely empty turn before the placeholder logic
// runs. Exercises the full production sequence via RunStream/NewLocalRuntime
// (not stripGeneratedMediaTransform called directly), so a regression that
// reorders the registered transforms in runtime.go's New would be caught
// here even though each transform's own unit test still passes in
// isolation.
func TestRunStream_MediaOnlyAssistantHistoryRemainsCoherent_UnknownModel(t *testing.T) {
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })

	turn1 := newStreamBuilder().
		AddMedia([]byte{0x89, 0x50, 0x4e, 0x47}, "image/png", "cat.png").
		AddStopWithUsage(1, 1).
		Build()
	turn2 := newStreamBuilder().AddContent("sure, noted").AddStopWithUsage(1, 1).Build()

	prov := &recordingMsgProvider{mockProvider: mockProvider{id: "test/mock-model"}}
	queue := []chat.MessageStream{turn1, turn2}
	prov.stream = turn1

	a := agent.New("root", "instructions", agent.WithModel(&queueRecordingProvider{recordingMsgProvider: prov, queue: queue}))
	tm := team.New(team.WithAgents(a))
	// mockModelStore.GetModel always returns (nil, nil): the "unknown model"
	// case, resolving to ModelCapabilities{} (no image/audio/video support)
	// — the same conservative default a genuinely capability-less model gets.
	r, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("draw a cat"), session.WithWorkingDir(t.TempDir()))
	for range r.RunStream(t.Context(), sess) {
	}

	sess.AddMessage(session.UserMessage("thanks"))
	for range r.RunStream(t.Context(), sess) {
	}

	require.Len(t, prov.got, 2, "the provider must have been called for both turns")

	secondTurnHistory := prov.got[1]
	require.GreaterOrEqual(t, len(secondTurnHistory), 3, "user, assistant, user must all be present")

	var assistantMsg *chat.Message
	for i := range secondTurnHistory {
		if secondTurnHistory[i].Role == chat.MessageRoleAssistant {
			assistantMsg = &secondTurnHistory[i]
		}
	}
	require.NotNil(t, assistantMsg,
		"the media-only assistant turn must still be present in history, not silently dropped by strip_unsupported_modalities running before the placeholder logic")
	assert.NotEmpty(t, assistantMsg.Content,
		"a media-only turn must carry a placeholder even for a capability-less/unknown model")
	assert.Contains(t, assistantMsg.Content, generatedMediaPlaceholderPrefix)
	for _, part := range assistantMsg.MultiContent {
		assert.NotEqual(t, chat.MessagePartTypeDocument, part.Type,
			"no document/media part must survive: both the no-resend policy and the capability strip must remove it")
	}
}

// queueRecordingProvider layers queueProvider's per-call stream rotation on
// top of recordingMsgProvider's message capture, so a two-turn test can both
// script distinct responses per turn and inspect what each turn sent.
type queueRecordingProvider struct {
	*recordingMsgProvider

	queue []chat.MessageStream
	calls int
}

func (p *queueRecordingProvider) CreateChatCompletionStream(ctx context.Context, msgs []chat.Message, tls []tools.Tool) (chat.MessageStream, error) {
	if p.calls < len(p.queue) {
		p.stream = p.queue[p.calls]
	}
	p.calls++
	return p.recordingMsgProvider.CreateChatCompletionStream(ctx, msgs, tls)
}
