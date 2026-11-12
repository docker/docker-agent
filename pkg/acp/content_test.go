package acp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/png"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/cache"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/internal/portcullistest"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func testPNGBase64(t *testing.T, w int) string {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, 1))))
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

func textResource(uri, mime, text string) acpsdk.ContentBlock {
	return acpsdk.ResourceBlock(acpsdk.EmbeddedResourceResource{TextResourceContents: &acpsdk.TextResourceContents{Uri: uri, MimeType: &mime, Text: text}})
}

func blobResource(uri, mime, data string) acpsdk.ContentBlock {
	return acpsdk.ResourceBlock(acpsdk.EmbeddedResourceResource{BlobResourceContents: &acpsdk.BlobResourceContents{Uri: uri, MimeType: &mime, Blob: data}})
}

func TestPromptDocumentsPreservePayloadAndOrder(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	pdf := []byte("%PDF-1.7\x00inline bytes")
	prompt := []acpsdk.ContentBlock{
		acpsdk.TextBlock("before"),
		textResource("file:///private/settings.json", "application/json", `{"answer":42}`),
		blobResource("file:///private/report.pdf", "application/pdf", base64.StdEncoding.EncodeToString(pdf)),
		textResource("file:///private/settings.json", "application/json", `{"answer":42}`),
		acpsdk.ImageBlock(testPNGBase64(t, 1), "image/png"),
		acpsdk.TextBlock("after"),
	}
	msg := a.buildUserMessage(t.Context(), "unused", prompt)
	assert.Equal(t, "beforeafter", msg.Message.Content)
	parts := msg.Message.MultiContent
	require.Len(t, parts, 6)
	assert.Equal(t, "before", parts[0].Text)
	assert.Equal(t, "after", parts[5].Text)
	assert.Equal(t, chat.Document{Name: "settings.json", MimeType: "application/json", Size: 13, Source: chat.DocumentSource{InlineText: `{"answer":42}`}}, *parts[1].Document)
	assert.Equal(t, chat.Document{Name: "report.pdf", MimeType: "application/pdf", Size: int64(len(pdf)), Source: chat.DocumentSource{InlineData: pdf}}, *parts[2].Document)
	assert.Equal(t, parts[1], parts[3])
	assert.NotSame(t, parts[1].Document, parts[3].Document)
	assert.Equal(t, "image/png", parts[4].Document.MimeType)
	assert.Equal(t, int64(len(parts[4].Document.Source.InlineData)), parts[4].Document.Size)
	saved, err := json.Marshal(msg)
	require.NoError(t, err)
	assert.NotContains(t, string(saved), "/private/")
	var restored session.Message
	require.NoError(t, json.Unmarshal(saved, &restored))
	assert.Equal(t, msg.Message.MultiContent, restored.Message.MultiContent)
}

func TestPromptDocumentMIMEDefaultsAndEmptyResources(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	msg := a.buildUserMessage(t.Context(), "unused", []acpsdk.ContentBlock{
		textResource("file:///empty.txt", "", ""),
		textResource("custom:private", "", "text"),
		blobResource("file:///notes.txt", "text/plain", base64.StdEncoding.EncodeToString([]byte("hello\n"))),
		blobResource("file:///data.json", "application/json", base64.StdEncoding.EncodeToString([]byte(`{"a":1}`))),
		blobResource("file:///file.pdf", "", base64.StdEncoding.EncodeToString([]byte("%PDF-1.7\n"))),
	})
	require.Len(t, msg.Message.MultiContent, 6)
	assert.Equal(t, "text/plain", msg.Message.MultiContent[0].Document.MimeType)
	assert.Empty(t, msg.Message.MultiContent[0].Document.Source.InlineText)
	assert.Equal(t, "resource", msg.Message.MultiContent[2].Document.Name)
	assert.Equal(t, "hello\n", msg.Message.MultiContent[3].Document.Source.InlineText)
	assert.Nil(t, msg.Message.MultiContent[3].Document.Source.InlineData)
	assert.Equal(t, `{"a":1}`, msg.Message.MultiContent[4].Document.Source.InlineText)
	assert.Equal(t, "application/pdf", msg.Message.MultiContent[5].Document.MimeType)
	plain := a.buildUserMessage(t.Context(), "unused", []acpsdk.ContentBlock{acpsdk.TextBlock("plain")})
	assert.Nil(t, plain.Message.MultiContent)
}

func TestPromptInvalidResourcesUseSafePlaceholder(t *testing.T) {
	t.Parallel()
	for _, block := range []acpsdk.ContentBlock{
		blobResource("file:///private/file.pdf", "application/pdf", "invalid base64!"),
		blobResource("file:///private/file.txt", "text/plain", base64.StdEncoding.EncodeToString([]byte{0xff})),
		textResource("file:///private/file.txt", "text/plain", strings.Repeat("x", chat.MaxInlineFileSize+1)),
		textResource("file:///private/file.txt", "invalid MIME", "body"),
		acpsdk.ImageBlock("AAAA", "image/png"),
	} {
		a := &Agent{}
		msg := a.buildUserMessage(t.Context(), "unused", []acpsdk.ContentBlock{acpsdk.TextBlock("before"), block, acpsdk.TextBlock("after")})
		assert.Contains(t, msg.Message.Content, "content unavailable")
		assert.True(t, strings.HasPrefix(msg.Message.Content, "before"))
		assert.True(t, strings.HasSuffix(msg.Message.Content, "after"))
		assert.NotContains(t, msg.Message.Content, "private")
		assert.Less(t, len(msg.Message.Content), 200)
		assert.Empty(t, msg.Message.MultiContent)
	}
	_, err := decodePromptData(strings.Repeat("A", base64.StdEncoding.EncodedLen(chat.MaxInlineBinarySize)+1))
	require.Error(t, err)
}

func TestPromptImageBoundsAndResize(t *testing.T) {
	t.Parallel()
	data, err := base64.StdEncoding.DecodeString(testPNGBase64(t, 1))
	require.NoError(t, err)
	// A valid header with enormous dimensions must fail before pixel decoding.
	binary.BigEndian.PutUint32(data[16:20], 100_000)
	binary.BigEndian.PutUint32(data[20:24], 100_000)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	_, _, err = promptDocument("image", "image/png", chat.DocumentSource{InlineData: data})
	require.ErrorContains(t, err, "dimensions too large")
	a := &Agent{}
	msg := a.buildUserMessage(t.Context(), "unused", []acpsdk.ContentBlock{acpsdk.ImageBlock(testPNGBase64(t, 2100), "image/png")})
	require.Len(t, msg.Message.MultiContent, 2)
	assert.NotNil(t, msg.Message.MultiContent[0].Document)
	assert.Contains(t, msg.Message.MultiContent[1].Text, "original 2100x1")
	assert.Contains(t, msg.Message.Content, "displayed at 2000x1")
}

func fileURI(path string) string {
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func TestResourceLinkPathDecodesOnce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"a b.txt", "a%20b.txt", "100%.txt", "%2e%2e.txt", "hash#query?.txt"} {
		expected := filepath.Join(root, name)
		got, ok := resourceLinkPath(fileURI(expected))
		require.True(t, ok)
		assert.Equal(t, expected, got)
	}
	for _, path := range []string{"relative.txt", "a%20b.txt", "100%.txt"} {
		got, ok := resourceLinkPath(path)
		require.True(t, ok)
		assert.Equal(t, path, got)
	}
	for _, uri := range []string{"", "file:///bad%zz", "file:relative", "file://remote/share/file", "file://user@localhost/path", "https://example.com/file", "file:////server/share/file", "file:///bad%00name", "//server/share/file"} {
		_, ok := resourceLinkPath(uri)
		assert.False(t, ok, uri)
	}
	got, ok := resourceLinkPath("file:///C:/work/a%2520b.txt")
	require.True(t, ok)
	if filepath.Separator == '\\' {
		assert.Equal(t, `C:\work\a%20b.txt`, got)
	} else {
		assert.Equal(t, "/C:/work/a%20b.txt", got)
	}
}

func TestResourceLinkReadsExactClientPathAndPreservesText(t *testing.T) {
	t.Parallel()
	fs, ctx, peer := newPolicyFileFixture(t, t.TempDir(), latest.Toolset{})
	peer.content = "unsaved buffer"
	uri := fileURI(filepath.Join(fs.workingDir, "a%20b.json"))
	link := acpsdk.ResourceLinkBlock("a%20b.json", uri)
	link.ResourceLink.MimeType = new("application/json")
	link.ResourceLink.Size = new(99999)
	msg := fs.agent.buildUserMessage(ctx, "policy-session", []acpsdk.ContentBlock{link})
	require.Len(t, msg.Message.MultiContent, 1)
	doc := msg.Message.MultiContent[0].Document
	require.NotNil(t, doc)
	assert.Equal(t, "unsaved buffer", doc.Source.InlineText)
	assert.Equal(t, "application/json", doc.MimeType)
	assert.Equal(t, int64(len("unsaved buffer")), doc.Size)
	root, err := filepath.EvalSymlinks(fs.workingDir)
	require.NoError(t, err)
	require.Len(t, peer.reads, 1)
	assert.Equal(t, filepath.Join(root, "a%20b.json"), peer.reads[0].Path)
}

func TestResourceLinkUnsafeAndCanceledNeverReads(t *testing.T) {
	t.Parallel()
	fs, ctx, peer := newPolicyFileFixture(t, t.TempDir(), latest.Toolset{})
	for _, uri := range []string{fileURI(filepath.Join(t.TempDir(), "secret.txt")), "https://example.com/private", "file:///invalid%zz", "file://remote/share/secret"} {
		msg := fs.agent.buildUserMessage(ctx, "policy-session", []acpsdk.ContentBlock{acpsdk.ResourceLinkBlock("safe", uri)})
		assert.Contains(t, msg.Message.Content, "content unavailable")
		assert.NotContains(t, msg.Message.Content, uri)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	fs.agent.buildUserMessage(canceled, "policy-session", []acpsdk.ContentBlock{acpsdk.ResourceLinkBlock("safe", fileURI(filepath.Join(fs.workingDir, "file.txt")))})
	reads, writes := peer.counts()
	assert.Zero(t, reads)
	assert.Zero(t, writes)
}

type contentTestProvider struct {
	mockProvider

	messages []chat.Message
	calls    int
}

func (p *contentTestProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.messages = messages
	p.calls++
	return &mockStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "Done"}}}},
		{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}},
	}}, nil
}

func TestPromptDocumentsReachProviderRedactedAndPersist(t *testing.T) {
	t.Parallel()
	secret := portcullistest.FakeGitHubPAT("cxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
	original := `{"token":"` + secret + `"}`
	prov := &contentTestProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "content")}}
	root := agent.New("root", "test", agent.WithModel(prov), agent.WithHooks(&latest.HooksConfig{
		BeforeLLMCall: []latest.HookDefinition{{Type: "builtin", Command: "redact_secrets"}},
	}))
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	rt, err := runtime.New(t.Context(), team.New(team.WithAgents(root)), runtime.WithSessionCompaction(false), runtime.WithSessionStore(store))
	require.NoError(t, err)
	defer func() { require.NoError(t, rt.Close()) }()
	a, s, _ := newPromptTestAgent(t, &fakeRuntime{})
	s.rt = rt
	require.NoError(t, store.AddSession(t.Context(), s.sess))
	req := promptRequest("")
	req.Prompt = []acpsdk.ContentBlock{textResource("file:///settings.json", "application/json", original), blobResource("file:///file.pdf", "application/pdf", base64.StdEncoding.EncodeToString([]byte("%PDF-1.7")))}
	resp, err := a.Prompt(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, acpsdk.StopReasonEndTurn, resp.StopReason)
	var found bool
	for _, msg := range prov.messages {
		for _, part := range msg.MultiContent {
			if part.Document != nil && part.Document.Name == "settings.json" {
				found = true
				assert.NotContains(t, part.Document.Source.InlineText, secret)
				assert.Equal(t, "application/json", part.Document.MimeType)
			}
		}
	}
	require.True(t, found)
	persisted, err := store.GetSession(t.Context(), s.sess.ID)
	require.NoError(t, err)
	var saved bool
	for _, msg := range persisted.OwnMessages() {
		for _, part := range msg.Message.MultiContent {
			if part.Document != nil && part.Document.Name == "settings.json" {
				saved = true
				assert.Equal(t, original, part.Document.Source.InlineText)
			}
		}
	}
	assert.True(t, saved, "model redaction must not mutate persisted user attachment")
}

func TestPromptDocumentsBypassTextResponseCache(t *testing.T) {
	t.Parallel()
	c, err := cache.New(cache.Config{Enabled: true})
	require.NoError(t, err)
	c.Store("summarize", "cached text-only answer")
	prov := &contentTestProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "attachments")}}
	root := agent.New("root", "test", agent.WithModel(prov), agent.WithCache(c))
	rt, err := runtime.New(t.Context(), team.New(team.WithAgents(root)), runtime.WithSessionCompaction(false))
	require.NoError(t, err)
	defer func() { require.NoError(t, rt.Close()) }()
	a, s, _ := newPromptTestAgent(t, &fakeRuntime{})
	s.rt = rt
	for _, text := range []string{"first resource", "different resource"} {
		req := promptRequest("summarize")
		req.Prompt = append(req.Prompt, textResource("file:///notes.txt", "text/plain", text))
		_, err := a.Prompt(t.Context(), req)
		require.NoError(t, err)
	}
	assert.Equal(t, 2, prov.calls, "different resources must each reach the provider")
	stored, ok := c.Lookup("summarize")
	require.True(t, ok)
	assert.Equal(t, "cached text-only answer", stored, "attachment responses must not overwrite the text-only cache")
	_, err = a.Prompt(t.Context(), promptRequest("summarize"))
	require.NoError(t, err)
	assert.Equal(t, 2, prov.calls, "a later text-only turn still uses the cache")
	assert.Equal(t, "cached text-only answer", s.sess.GetLastAssistantMessageContent())
}

func TestPromptMediaMIMETextSurvivesRuntimeFiltering(t *testing.T) {
	t.Parallel()
	prov := &contentTestProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "text-only")}}
	root := agent.New("root", "test", agent.WithModel(prov))
	rt, err := runtime.New(t.Context(), team.New(team.WithAgents(root)), runtime.WithSessionCompaction(false))
	require.NoError(t, err)
	defer func() { require.NoError(t, rt.Close()) }()
	a, s, _ := newPromptTestAgent(t, &fakeRuntime{})
	s.rt = rt
	req := promptRequest("")
	req.Prompt = []acpsdk.ContentBlock{textResource("file:///drawing.svg", "image/svg+xml", "<svg>text</svg>"), textResource("file:///empty.txt", "text/plain", "")}
	_, err = a.Prompt(t.Context(), req)
	require.NoError(t, err)
	encoded, err := json.Marshal(prov.messages)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), "image/svg+xml")
	var svgFound bool
	for _, msg := range prov.messages {
		for _, part := range msg.MultiContent {
			if part.Document != nil && part.Document.MimeType == "image/svg+xml" {
				svgFound = true
				assert.Equal(t, "<svg>text</svg>", part.Document.Source.InlineText)
			}
		}
	}
	assert.True(t, svgFound)

	assert.Contains(t, string(encoded), "empty.txt (empty)")
}

func TestPromptParameterizedBinaryMIME(t *testing.T) {
	t.Parallel()
	doc, _, err := promptDocument("file.pdf", "Application/PDF; version=1.7", chat.DocumentSource{InlineData: []byte("%PDF-1.7")})
	require.NoError(t, err)
	assert.Equal(t, "application/pdf", doc.MimeType)
}

func TestPromptImageUsesActualMIME(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	msg := a.buildUserMessage(t.Context(), "unused", []acpsdk.ContentBlock{acpsdk.ImageBlock(testPNGBase64(t, 1), "image/jpeg")})
	require.Len(t, msg.Message.MultiContent, 1)
	require.NotNil(t, msg.Message.MultiContent[0].Document)
	assert.Equal(t, "image/png", msg.Message.MultiContent[0].Document.MimeType)
}

func TestToolCompletionDoesNotBypassTextTransformsWithAttachments(t *testing.T) {
	t.Parallel()
	event := runtime.ToolCallResponse("call", tools.Tool{Name: "mcp"}, &tools.ToolCallResult{
		Output: "raw secret", Images: []tools.MediaContent{{MimeType: "image/png", Data: "private image"}},
		Documents: []tools.DocumentContent{{URI: "file:///private", Text: "private document"}}, StructuredContent: map[string]any{"secret": "private structured content"},
	}, "redacted", "root").(*runtime.ToolCallResponseEvent)
	update := buildToolCallComplete(event)
	encoded, err := json.Marshal(update)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "private")
	assert.NotContains(t, string(encoded), "raw secret")
	require.Len(t, update.ToolCallUpdate.Content, 1)
	assert.Equal(t, "redacted", update.ToolCallUpdate.Content[0].Content.Content.Text.Text)
}
