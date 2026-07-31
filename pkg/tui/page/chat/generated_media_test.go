package chat

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// resolverResult is one canned outcome of the fake resolver runtime, keyed
// by the reference's path.
type resolverResult struct {
	data []byte
	path string
	err  error
}

// resolverTestRuntime is queueTestRuntime plus the local-runtime-only
// generated-file resolver capability (see pkg/app's generatedFileResolver),
// with canned per-path outcomes and a record of every resolved ref. The
// real manifest/workspace security behind the capability is covered in
// pkg/runtime; these tests pin the TUI contract around it.
type resolverTestRuntime struct {
	queueTestRuntime

	mu      sync.Mutex
	results map[string]resolverResult
	refs    []runtime.GeneratedFileRef
}

func (r *resolverTestRuntime) ResolveGeneratedFile(_ context.Context, ref runtime.GeneratedFileRef) (*runtime.ResolvedGeneratedFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = append(r.refs, ref)
	res, ok := r.results[ref.Path]
	if !ok || res.err != nil {
		return nil, runtime.ErrGeneratedFileUnavailable
	}
	return &runtime.ResolvedGeneratedFile{Data: res.data, Path: res.path}, nil
}

func (r *resolverTestRuntime) resolveCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.refs)
}

// mediaRecordingMessages wraps the real [messages.Model], recording
// AppendAssistantMedia and UpdateAssistantMedia calls while forwarding them
// so the real list state mutates. Mirrors recordingMessages in
// image_output_guard_integration_test.go.
type mediaRecordingMessages struct {
	messages.Model

	mediaAgents  []string
	mediaCalls   [][]types.AssistantMedia
	mediaUpdates [][]types.AssistantMedia
}

func (r *mediaRecordingMessages) AppendAssistantMedia(agentName string, media []types.AssistantMedia) tea.Cmd {
	r.mediaAgents = append(r.mediaAgents, agentName)
	r.mediaCalls = append(r.mediaCalls, media)
	return r.Model.AppendAssistantMedia(agentName, media)
}

func (r *mediaRecordingMessages) UpdateAssistantMedia(media []types.AssistantMedia) tea.Cmd {
	r.mediaUpdates = append(r.mediaUpdates, media)
	return r.Model.UpdateAssistantMedia(media)
}

func newGeneratedMediaTestPage(t *testing.T, rt runtime.Runtime) (*chatPage, *mediaRecordingMessages) {
	t.Helper()
	return newGeneratedMediaTestPageWithSession(t, rt, session.New())
}

func testPNGBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{B: 255, A: 255})
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, img))
	return data.Bytes()
}

func newGeneratedMediaTestPageWithSession(t *testing.T, rt runtime.Runtime, sess *session.Session) (*chatPage, *mediaRecordingMessages) {
	t.Helper()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), rt, sess), service.NewSessionState(sess)).(*chatPage)
	rec := &mediaRecordingMessages{Model: p.messages}
	p.messages = rec
	return p, rec
}

// workspaceImagePart builds the document part a MessageAddedEvent carries
// for a workspace-materialized generated image.
func workspaceImagePart(name, relPath, owner string) chat.MessagePart {
	return chat.MessagePart{
		Type: chat.MessagePartTypeDocument,
		Document: &chat.Document{
			Name:     name,
			MimeType: "image/png",
			Source: chat.DocumentSource{
				ArtifactPath:           relPath,
				ArtifactRoot:           chat.ArtifactRootWorkspace,
				ArtifactOwnerSessionID: owner,
			},
		},
	}
}

// assistantMessageAdded builds the event the run loop emits after
// persisting the "root" agent's assistant message.
func assistantMessageAdded(sessionID string, parts ...chat.MessagePart) *runtime.MessageAddedEvent {
	msg := &session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:         chat.MessageRoleAssistant,
			MultiContent: parts,
		},
	}
	return runtime.MessageAdded(sessionID, msg, "root").(*runtime.MessageAddedEvent)
}

// resolveArmedMedia runs the asynchronous resolution command the page armed
// (recorded like a routed timer, so it survives background-tab dispatch)
// and returns the resolved-media message it produced.
func resolveArmedMedia(t *testing.T, p *chatPage) generatedMediaResolvedMsg {
	t.Helper()
	cmd := p.TakeRoutedTimers()
	require.NotNil(t, cmd, "an async resolution command must be armed")
	for _, msg := range runTimerCmd(t, cmd) {
		if resolved, ok := msg.(generatedMediaResolvedMsg); ok {
			return resolved
		}
	}
	t.Fatal("the armed commands produced no generatedMediaResolvedMsg")
	return generatedMediaResolvedMsg{}
}

func TestMessageAdded_TextAndWorkspaceImageJoinSameTurn(t *testing.T) {
	t.Parallel()

	const owner = "sess-media"
	rt := &resolverTestRuntime{results: map[string]resolverResult{
		"cat.png": {data: testPNGBytes(t), path: "/workspace/cat.png"},
	}}
	p, rec := newGeneratedMediaTestPage(t, rt)

	rec.AddUserMessage("draw a cat")
	handled, _ := p.handleRuntimeEvent(runtime.StreamStarted(owner, "root"))
	require.True(t, handled)
	handled, _ = p.handleRuntimeEvent(runtime.AgentChoice("root", owner, "Here is your cat:"))
	require.True(t, handled)
	require.Equal(t, 1, rec.MessageTypeCount(types.MessageTypeAssistant))

	handled, _ = p.handleRuntimeEvent(assistantMessageAdded(owner,
		chat.MessagePart{Type: chat.MessagePartTypeText, Text: "Here is your cat:"},
		workspaceImagePart("cat.png", "cat.png", owner),
	))
	require.True(t, handled, "MessageAddedEvent must be a recognized runtime event")

	// The placeholder is attached synchronously; resolution must not have
	// happened inside the update path.
	require.Equal(t, []string{"root"}, rec.mediaAgents)
	require.Len(t, rec.mediaCalls, 1)
	require.Len(t, rec.mediaCalls[0], 1)
	placeholder := rec.mediaCalls[0][0]
	assert.Nil(t, placeholder.Image, "nothing is decoded before the async resolution")
	assert.Equal(t, `Generated image "cat.png" is unavailable.`, placeholder.Fallback)
	assert.NotZero(t, placeholder.ID, "a resolvable item must carry a replacement ID")
	assert.Zero(t, rt.resolveCalls(), "the resolver must never run synchronously inside Update")

	resolved := resolveArmedMedia(t, p)
	_, _ = p.update(resolved)

	assert.Equal(t, 1, rt.resolveCalls())
	require.Len(t, rec.mediaUpdates, 1)
	media := rec.mediaUpdates[0][0]
	assert.Equal(t, placeholder.ID, media.ID)
	require.NotNil(t, media.Image, "a resolvable file must be decoded for inline rendering")
	assert.Equal(t, "cat.png", media.Image.Name)
	assert.NotEmpty(t, media.Image.PNGData)
	assert.Equal(t, `Generated image "cat.png" saved to: /workspace/cat.png`, media.Fallback,
		"the fallback must surface the resolver-validated canonical workspace path")

	assert.Equal(t, 1, rec.MessageTypeCount(types.MessageTypeAssistant),
		"the media must join the streamed-text assistant message, not open a new turn")
	assert.Zero(t, rec.MessageTypeCount(types.MessageTypeSpinner))
	assert.True(t, p.hasReceivedAssistantContent)
}

// TestMessageAdded_LabelsUseFinalPersistedDocumentName is the live-repro
// pin for prompt-directed naming ("Generate an image of a red panda coding
// at a terminal as assets/red-panda-terminal.jpg"): the same-turn inline
// label and fallback must carry the FINAL persisted Document name and the
// resolver's canonical workspace path — after marker/prompt naming, MIME
// extension correction, and collision suffixing — never a provisional or
// TUI-constructed one. The TUI gets both exclusively from the trusted
// runtime: the name from the MessageAddedEvent document, the path from
// ResolveGeneratedFile.
func TestMessageAdded_LabelsUseFinalPersistedDocumentName(t *testing.T) {
	t.Parallel()

	const owner = "sess-final-name"
	rt := &resolverTestRuntime{results: map[string]resolverResult{
		"assets/red-panda-terminal.png":   {data: testPNGBytes(t), path: "/workspace/assets/red-panda-terminal.png"},
		"assets/red-panda-terminal-1.png": {data: testPNGBytes(t), path: "/workspace/assets/red-panda-terminal-1.png"},
	}}
	p, rec := newGeneratedMediaTestPage(t, rt)

	handled, _ := p.handleRuntimeEvent(assistantMessageAdded(owner,
		chat.MessagePart{Type: chat.MessagePartTypeText, Text: "Here is your red panda coding at a terminal:"},
		workspaceImagePart("red-panda-terminal.png", "assets/red-panda-terminal.png", owner),
		workspaceImagePart("red-panda-terminal-1.png", "assets/red-panda-terminal-1.png", owner),
	))
	require.True(t, handled)

	require.Len(t, rec.mediaCalls, 1)
	require.Len(t, rec.mediaCalls[0], 2)
	assert.Equal(t, `Generated image "red-panda-terminal.png" is unavailable.`, rec.mediaCalls[0][0].Fallback,
		"even the pre-resolution placeholder must name the final persisted file")
	assert.Equal(t, `Generated image "red-panda-terminal-1.png" is unavailable.`, rec.mediaCalls[0][1].Fallback,
		"a collision-suffixed final name must be shown as persisted")

	_, _ = p.update(resolveArmedMedia(t, p))
	require.Len(t, rec.mediaUpdates, 1)
	require.Len(t, rec.mediaUpdates[0], 2)

	resolved := rec.mediaUpdates[0][0]
	require.NotNil(t, resolved.Image)
	assert.Equal(t, "red-panda-terminal.png", resolved.Image.Name,
		"the inline label must be the final persisted document name, not a provisional generated-N one")
	assert.Equal(t, `Generated image "red-panda-terminal.png" saved to: /workspace/assets/red-panda-terminal.png`, resolved.Fallback,
		"the fallback must carry the resolver-validated canonical workspace path")

	suffixed := rec.mediaUpdates[0][1]
	require.NotNil(t, suffixed.Image)
	assert.Equal(t, "red-panda-terminal-1.png", suffixed.Image.Name)
	assert.Equal(t, `Generated image "red-panda-terminal-1.png" saved to: /workspace/assets/red-panda-terminal-1.png`, suffixed.Fallback)
}

func TestMessageAdded_MediaOnlyTurnReplacesSpinner(t *testing.T) {
	t.Parallel()

	const owner = "sess-media-only"
	rt := &resolverTestRuntime{results: map[string]resolverResult{
		"cat.png": {data: testPNGBytes(t), path: "/workspace/cat.png"},
	}}
	p, rec := newGeneratedMediaTestPage(t, rt)

	rec.AddUserMessage("draw a cat")
	_, _ = p.handleRuntimeEvent(runtime.StreamStarted(owner, "root"))
	require.Equal(t, 1, rec.MessageTypeCount(types.MessageTypeSpinner),
		"a real pending spinner must exist before the media arrives")

	handled, _ := p.handleRuntimeEvent(assistantMessageAdded(owner, workspaceImagePart("cat.png", "cat.png", owner)))
	require.True(t, handled)

	assert.Zero(t, rec.MessageTypeCount(types.MessageTypeSpinner),
		"a media-only turn must replace the pending spinner immediately, before resolution")
	require.Equal(t, 1, rec.MessageTypeCount(types.MessageTypeAssistant),
		"a media-only turn must add a visible assistant message")
	require.Len(t, rec.mediaCalls, 1)
	assert.True(t, p.hasReceivedAssistantContent, "media-only output counts as assistant content")

	_, _ = p.update(resolveArmedMedia(t, p))
	require.Len(t, rec.mediaUpdates, 1)
	require.NotNil(t, rec.mediaUpdates[0][0].Image)
}

func TestMessageAdded_PreservesMediaOrder(t *testing.T) {
	t.Parallel()

	const owner = "sess-order"
	rt := &resolverTestRuntime{results: map[string]resolverResult{
		"first.png":  {data: testPNGBytes(t), path: "/workspace/first.png"},
		"second.png": {data: testPNGBytes(t), path: "/workspace/second.png"},
	}}
	p, rec := newGeneratedMediaTestPage(t, rt)

	handled, _ := p.handleRuntimeEvent(assistantMessageAdded(owner,
		chat.MessagePart{Type: chat.MessagePartTypeText, Text: "two images"},
		workspaceImagePart("first.png", "first.png", owner),
		workspaceImagePart("second.png", "second.png", owner),
	))
	require.True(t, handled)

	require.Len(t, rec.mediaCalls, 1)
	require.Len(t, rec.mediaCalls[0], 2)

	_, _ = p.update(resolveArmedMedia(t, p))
	require.Len(t, rec.mediaUpdates, 1)
	require.Len(t, rec.mediaUpdates[0], 2)
	assert.Equal(t, "first.png", rec.mediaUpdates[0][0].Image.Name)
	assert.Equal(t, "second.png", rec.mediaUpdates[0][1].Image.Name)
	assert.Equal(t, rec.mediaCalls[0][0].ID, rec.mediaUpdates[0][0].ID,
		"resolved items must target their placeholders in order")
	assert.Equal(t, rec.mediaCalls[0][1].ID, rec.mediaUpdates[0][1].ID)
}

// TestMessageAdded_NilMessageIsNoOp pins the remote-runtime shape: the
// Message payload is process-local (json:"-"), so a decoded remote event
// carries only IDs. It must be a defined no-op — no panic, no resolution
// attempt, no list mutation.
func TestMessageAdded_NilMessageIsNoOp(t *testing.T) {
	t.Parallel()

	p, rec := newGeneratedMediaTestPage(t, &resolverTestRuntime{})
	_, _ = p.handleRuntimeEvent(runtime.StreamStarted("sess-remote", "root"))
	spinners := rec.MessageTypeCount(types.MessageTypeSpinner)

	var handled bool
	require.NotPanics(t, func() {
		handled, _ = p.handleRuntimeEvent(runtime.MessageAdded("sess-remote", nil, "root"))
	})
	assert.True(t, handled)
	assert.Empty(t, rec.mediaCalls, "a payload-less event must not produce media")
	assert.Equal(t, spinners, rec.MessageTypeCount(types.MessageTypeSpinner), "the pending spinner must be untouched")
	assert.False(t, p.hasReceivedAssistantContent)
}

// TestMessageAdded_NoResolverCapabilityIsNoOp covers runtimes that cannot
// resolve generated files (e.g. remote runtimes): even a fully
// workspace-backed payload must render nothing rather than guess.
func TestMessageAdded_NoResolverCapabilityIsNoOp(t *testing.T) {
	t.Parallel()

	p, rec := newGeneratedMediaTestPage(t, queueTestRuntime{})

	handled, cmd := p.handleRuntimeEvent(assistantMessageAdded("sess-no-cap",
		workspaceImagePart("cat.png", "cat.png", "sess-no-cap")))
	assert.True(t, handled)
	assert.Nil(t, cmd)
	assert.Empty(t, rec.mediaCalls)
	assert.Nil(t, p.TakeRoutedTimers(), "no resolution may be armed without the capability")
}

func TestMessageAdded_UnresolvableFallsBackToFilenameOnly(t *testing.T) {
	t.Parallel()

	const owner = "sess-missing"
	rt := &resolverTestRuntime{} // every resolution fails
	p, rec := newGeneratedMediaTestPage(t, rt)

	handled, _ := p.handleRuntimeEvent(assistantMessageAdded(owner,
		workspaceImagePart("cat.png", "missing-file.png", owner)))
	require.True(t, handled)

	_, _ = p.update(resolveArmedMedia(t, p))

	require.Len(t, rec.mediaUpdates, 1)
	media := rec.mediaUpdates[0][0]
	assert.Nil(t, media.Image, "an unresolvable file has nothing to render")
	assert.Equal(t, `Generated image "cat.png" is unavailable.`, media.Fallback,
		"an unresolved file falls back to the display filename only — no guessed path")
	assert.NotContains(t, media.Fallback, owner, "the owner session ID must never leak into the fallback")
	assert.NotContains(t, media.Fallback, "missing-file.png", "the raw reference must never leak into the fallback")
	assert.NotContains(t, media.Fallback, "workspace", "no root kind or path may be shown for an unresolved file")
	assert.NotContains(t, media.Fallback, "unavailable:", "raw resolver errors must never leak into the fallback")
}

func TestMessageAdded_UndecodableFallsBackToCanonicalPath(t *testing.T) {
	t.Parallel()

	const owner = "sess-corrupt"
	rt := &resolverTestRuntime{results: map[string]resolverResult{
		"cat.png": {data: []byte("not really a png"), path: "/workspace/cat.png"},
	}}
	p, rec := newGeneratedMediaTestPage(t, rt)

	handled, _ := p.handleRuntimeEvent(assistantMessageAdded(owner, workspaceImagePart("cat.png", "cat.png", owner)))
	require.True(t, handled)
	_, _ = p.update(resolveArmedMedia(t, p))

	require.Len(t, rec.mediaUpdates, 1)
	media := rec.mediaUpdates[0][0]
	assert.Nil(t, media.Image, "undecodable bytes must not be handed to the renderer")
	assert.Equal(t, `Generated image "cat.png" saved to: /workspace/cat.png`, media.Fallback,
		"a resolved-but-undecodable file must surface its validated canonical path")
}

// TestMessageAdded_ControlCharPathStaysUnavailable: a canonical path that
// cannot be shown verbatim (control characters) must degrade to the
// unavailable wording, never reach the terminal.
func TestMessageAdded_ControlCharPathStaysUnavailable(t *testing.T) {
	t.Parallel()

	const owner = "sess-hostile-path"
	rt := &resolverTestRuntime{results: map[string]resolverResult{
		"cat.png": {data: []byte("not a png"), path: "/workspace/\x1b[31mcat.png"},
	}}
	p, rec := newGeneratedMediaTestPage(t, rt)

	_, _ = p.handleRuntimeEvent(assistantMessageAdded(owner, workspaceImagePart("cat.png", "cat.png", owner)))
	_, _ = p.update(resolveArmedMedia(t, p))

	require.Len(t, rec.mediaUpdates, 1)
	fallback := rec.mediaUpdates[0][0].Fallback
	assert.Equal(t, `Generated image "cat.png" is unavailable.`, fallback)
	assert.NotContains(t, fallback, "\x1b")
}

func TestMessageAdded_SanitizesHostileDisplayName(t *testing.T) {
	t.Parallel()

	const owner = "sess-hostile"
	rt := &resolverTestRuntime{} // resolution fails; only the name reaches the fallback
	p, rec := newGeneratedMediaTestPage(t, rt)

	_, _ = p.handleRuntimeEvent(assistantMessageAdded(owner,
		workspaceImagePart("../evil/<img>\x1b[31mname.png", "cat.png", owner)))
	_, _ = p.update(resolveArmedMedia(t, p))

	require.Len(t, rec.mediaUpdates, 1)
	fallback := rec.mediaUpdates[0][0].Fallback
	assert.NotContains(t, fallback, "..", "traversal-like sequences must be sanitized out of the display name")
	assert.NotContains(t, fallback, "<img>", "angle brackets must be sanitized out of the display name")
	assert.NotContains(t, fallback, "\x1b", "control characters must never reach the terminal")
	assert.Contains(t, fallback, "name.png")
}

func TestMessageAdded_IgnoresNonGeneratedAndNonImageParts(t *testing.T) {
	t.Parallel()

	const owner = "sess-skip"
	rt := &resolverTestRuntime{}
	p, rec := newGeneratedMediaTestPage(t, rt)

	pdf := workspaceImagePart("doc.pdf", "doc.pdf", owner)
	pdf.Document.MimeType = "application/pdf"

	handled, cmd := p.handleRuntimeEvent(assistantMessageAdded(owner,
		chat.MessagePart{Type: chat.MessagePartTypeText, Text: "just text"},
		// User-attached image: inline bytes, no generated-file reference.
		chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
			Name:     "attached.png",
			MimeType: "image/png",
			Source:   chat.DocumentSource{InlineData: testPNGBytes(t)},
		}},
		// Ownerless reference: never resolved against a guessed session.
		chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
			Name:     "legacy.png",
			MimeType: "image/png",
			Source:   chat.DocumentSource{ArtifactPath: "legacy.png"},
		}},
		pdf,
	))
	assert.True(t, handled)
	assert.Nil(t, cmd)
	assert.Empty(t, rec.mediaCalls, "no part above is a generated workspace-backed image")
	assert.Zero(t, rt.resolveCalls())
}

// TestMessageAdded_UnknownRootRefStaysUnavailable: references whose root
// kind is unknown (empty ArtifactRoot, owner present) show the sanitized
// unavailable fallback without ever hitting the resolver.
func TestMessageAdded_UnknownRootRefStaysUnavailable(t *testing.T) {
	t.Parallel()

	const owner = "sess-legacy"
	rt := &resolverTestRuntime{}
	p, rec := newGeneratedMediaTestPage(t, rt)

	part := workspaceImagePart("cat.png", "cat.png", owner)
	part.Document.Source.ArtifactRoot = ""
	handled, _ := p.handleRuntimeEvent(assistantMessageAdded(owner, part))
	require.True(t, handled)

	require.Len(t, rec.mediaCalls, 1)
	media := rec.mediaCalls[0][0]
	assert.Zero(t, media.ID, "an unknown-root item is final: nothing will replace it")
	assert.Nil(t, media.Image)
	assert.Equal(t, `Generated image "cat.png" is unavailable.`, media.Fallback)
	assert.Nil(t, p.TakeRoutedTimers(), "no resolution may be armed for an unknown-root reference")
	assert.Zero(t, rt.resolveCalls())
}

func TestMessageAdded_NonAssistantRoleIsNoOp(t *testing.T) {
	t.Parallel()

	const owner = "sess-role"
	p, rec := newGeneratedMediaTestPage(t, &resolverTestRuntime{})

	msg := &session.Message{AgentName: "root", Message: chat.Message{
		Role:         chat.MessageRoleTool,
		MultiContent: []chat.MessagePart{workspaceImagePart("cat.png", "cat.png", owner)},
	}}
	handled, cmd := p.handleRuntimeEvent(runtime.MessageAdded(owner, msg, "root"))
	assert.True(t, handled)
	assert.Nil(t, cmd)
	assert.Empty(t, rec.mediaCalls)
}

func TestMessageAdded_CancelledStreamIsNoOp(t *testing.T) {
	t.Parallel()

	const owner = "sess-cancelled"
	p, rec := newGeneratedMediaTestPage(t, &resolverTestRuntime{})
	p.streamCancelled = true

	handled, cmd := p.handleRuntimeEvent(assistantMessageAdded(owner, workspaceImagePart("cat.png", "cat.png", owner)))
	assert.True(t, handled)
	assert.Nil(t, cmd)
	assert.Empty(t, rec.mediaCalls, "no media may be appended after the user cancelled the stream")
}

// restoredMediaSession builds a persisted session whose SECOND assistant
// message carries generated media, so targeting the right historical
// message (not the newest) is exercised.
func restoredMediaSession(owner string) *session.Session {
	sess := session.New()
	sess.ID = owner
	sess.Messages = []session.Item{
		session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "draw a cat"}}),
		session.NewMessageItem(&session.Message{AgentName: "root", Message: chat.Message{
			Role: chat.MessageRoleAssistant, Content: "Working on it.",
		}}),
		session.NewMessageItem(&session.Message{AgentName: "root", Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Here is your cat:",
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "Here is your cat:"},
				workspaceImagePart("cat.png", "cat.png", owner),
			},
		}}),
		session.NewMessageItem(&session.Message{AgentName: "root", Message: chat.Message{
			Role: chat.MessageRoleAssistant, Content: "Anything else?",
		}}),
	}
	return sess
}

// TestInit_RestoredSessionResolvesGeneratedMedia: a restored session's
// generated media is attached at load (sanitized placeholder) and resolved
// asynchronously, exactly like the live path.
func TestInit_RestoredSessionResolvesGeneratedMedia(t *testing.T) {
	t.Parallel()

	const owner = "sess-restored"
	rt := &resolverTestRuntime{results: map[string]resolverResult{
		"cat.png": {data: testPNGBytes(t), path: "/workspace/cat.png"},
	}}
	p, rec := newGeneratedMediaTestPageWithSession(t, rt, restoredMediaSession(owner))

	_ = p.Init()

	require.Equal(t, 3, rec.MessageTypeCount(types.MessageTypeAssistant))
	assert.Zero(t, rt.resolveCalls(), "restoring a session must not resolve synchronously")

	_, _ = p.update(resolveArmedMedia(t, p))

	assert.Equal(t, 1, rt.resolveCalls())
	assert.Equal(t, []runtime.GeneratedFileRef{{
		OwnerSessionID: owner,
		Root:           chat.ArtifactRootWorkspace,
		Path:           "cat.png",
	}}, rt.refs, "the persisted owner reference must be resolved as-is")
	require.Len(t, rec.mediaUpdates, 1)
	media := rec.mediaUpdates[0][0]
	require.NotNil(t, media.Image)
	assert.Equal(t, `Generated image "cat.png" saved to: /workspace/cat.png`, media.Fallback)
}

// TestCollectRestoredGeneratedMedia_TargetsOwningMessage pins the position
// mapping LoadFromSession consumes: media lands on the exact session index
// of the assistant message that carries the reference.
func TestCollectRestoredGeneratedMedia_TargetsOwningMessage(t *testing.T) {
	t.Parallel()

	const owner = "sess-positions"
	sess := restoredMediaSession(owner)
	p, _ := newGeneratedMediaTestPageWithSession(t, &resolverTestRuntime{}, sess)

	restored, requests := p.collectRestoredGeneratedMedia(sess)

	require.Len(t, restored, 1)
	require.Len(t, restored[2], 1, "the media must be keyed to the carrying message's session position")
	assert.Equal(t, `Generated image "cat.png" is unavailable.`, restored[2][0].Fallback)
	require.Len(t, requests, 1)
	assert.Equal(t, restored[2][0].ID, requests[0].id)
}

// TestCollectRestoredGeneratedMedia_NoCapability: without the resolver
// capability (remote runtimes) a restored session renders no media at all —
// the pre-resolver behavior.
func TestCollectRestoredGeneratedMedia_NoCapability(t *testing.T) {
	t.Parallel()

	const owner = "sess-remote-restore"
	sess := restoredMediaSession(owner)
	p, rec := newGeneratedMediaTestPageWithSession(t, queueTestRuntime{}, sess)

	restored, requests := p.collectRestoredGeneratedMedia(sess)
	assert.Nil(t, restored)
	assert.Nil(t, requests)

	_ = p.Init()
	require.Equal(t, 3, rec.MessageTypeCount(types.MessageTypeAssistant))
	assert.Nil(t, p.TakeRoutedTimers())
}
