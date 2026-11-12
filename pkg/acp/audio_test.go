package acp

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/cache"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
)

func TestPromptAudioPreservesBytesMIMEAndOrder(t *testing.T) {
	t.Parallel()
	data := []byte{0, 1, 2, 255, 254}
	block := acpsdk.AudioBlock(base64.StdEncoding.EncodeToString(data), "Audio/PCM; RATE=24000")
	block.Audio.Meta = map[string]any{"private": "not stored"}
	a := &Agent{}
	msg := a.buildUserMessage(t.Context(), "unused", []acpsdk.ContentBlock{acpsdk.TextBlock("before"), block, textResource("file:///notes.txt", "text/plain", "middle"), block, acpsdk.TextBlock("after")})
	require.Len(t, msg.Message.MultiContent, 7)
	assert.Equal(t, "before"+promptAudioNote+promptAudioNote+"after", msg.Message.Content)
	for _, i := range []int{1, 4} {
		assert.Equal(t, chat.MessagePartTypeDocument, msg.Message.MultiContent[i].Type)
		assert.Equal(t, chat.Document{Name: "audio", MimeType: "audio/pcm; rate=24000", Size: 5, Source: chat.DocumentSource{InlineData: data}}, *msg.Message.MultiContent[i].Document)
		assert.Equal(t, promptAudioNote, msg.Message.MultiContent[i+1].Text)
	}
	assert.NotSame(t, msg.Message.MultiContent[1].Document, msg.Message.MultiContent[4].Document)
	assert.Equal(t, "middle", msg.Message.MultiContent[3].Document.Source.InlineText)
	saved, err := json.Marshal(msg)
	require.NoError(t, err)
	assert.NotContains(t, string(saved), "private")
	var restored session.Message
	require.NoError(t, json.Unmarshal(saved, &restored))
	assert.Equal(t, msg.Message, restored.Message)
}

func TestPromptAudioInvalidAndSizeBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ data, mime string }{
		{"AAAA", "text/plain"},
		{"AAAA", "audio/*"},
		{"AAAA", "audio/"},
		{"AAAA", "invalid MIME"},
		{"AAAA", "audio/wav; bad"},
		{"AAAA", "audio/wav; private=" + strings.Repeat("x", 257)},
		{"", "audio/wav"},
		{"\r\n", "audio/wav"},
		{"secret-invalid-data!", "audio/wav"},
		{"data:audio/wav;base64,AAAA", "audio/wav"},
	} {
		msg := (&Agent{}).buildUserMessage(t.Context(), "unused", []acpsdk.ContentBlock{acpsdk.TextBlock("before"), acpsdk.AudioBlock(tc.data, tc.mime), acpsdk.TextBlock("after")})
		assert.Equal(t, "before[Attachment: audio (content unavailable)]after", msg.Message.Content)
		assert.Empty(t, msg.Message.MultiContent)
	}
	data := make([]byte, chat.MaxInlineBinarySize)
	encoded := base64.StdEncoding.EncodeToString(data)
	msg := (&Agent{}).buildUserMessage(t.Context(), "unused", []acpsdk.ContentBlock{acpsdk.AudioBlock(encoded, "audio/wav")})
	require.Len(t, msg.Message.MultiContent, 2)
	assert.EqualValues(t, chat.MaxInlineBinarySize, msg.Message.MultiContent[0].Document.Size)
	msg = (&Agent{}).buildUserMessage(t.Context(), "unused", []acpsdk.ContentBlock{acpsdk.AudioBlock(encoded+"AAAA", "audio/wav")})
	assert.Empty(t, msg.Message.MultiContent)
	assert.Equal(t, "[Attachment: audio (content unavailable)]", msg.Message.Content)
}

type audioTestProvider struct {
	contentTestProvider

	audio bool
}

func (p *audioTestProvider) BaseConfig() base.Config {
	return base.Config{ModelConfig: latest.ModelConfig{Capabilities: &latest.CapabilitiesConfig{Audio: p.audio}}}
}

func TestPromptAudioProviderFilteringPersistenceAndCache(t *testing.T) {
	t.Parallel()
	for _, supported := range []bool{false, true} {
		name := "unsupported"
		if supported {
			name = "supported"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			prov := &audioTestProvider{contentTestProvider: contentTestProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "audio")}}, audio: supported}
			c, err := cache.New(cache.Config{Enabled: true})
			require.NoError(t, err)
			c.Store(promptAudioNote, "cached text-only answer")
			root := agent.New("root", "test", agent.WithModel(prov), agent.WithCache(c))
			store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
			require.NoError(t, err)
			defer func() { require.NoError(t, store.Close()) }()
			rt, err := runtime.New(t.Context(), team.New(team.WithAgents(root)), runtime.WithSessionStore(store), runtime.WithSessionCompaction(false))
			require.NoError(t, err)
			defer func() { require.NoError(t, rt.Close()) }()
			a, s, _ := newPromptTestAgent(t, &fakeRuntime{})
			s.rt = rt
			require.NoError(t, store.AddSession(t.Context(), s.sess))
			for _, payload := range []string{"first audio bytes", "second audio bytes"} {
				req := promptRequest("")
				req.Prompt = []acpsdk.ContentBlock{acpsdk.AudioBlock(base64.StdEncoding.EncodeToString([]byte(payload)), "audio/wav")}
				response, err := a.Prompt(t.Context(), req)
				require.NoError(t, err)
				assert.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
			}
			assert.Equal(t, 2, prov.calls)
			saved, err := store.GetSession(t.Context(), s.sess.ID)
			require.NoError(t, err)
			var storedAudio int
			for _, msg := range saved.OwnMessages() {
				for _, part := range msg.Message.MultiContent {
					if part.Document != nil {
						storedAudio++
						assert.Equal(t, "audio/wav", part.Document.MimeType)
						assert.NotEmpty(t, part.Document.Source.InlineData)
					}
				}
			}
			assert.Equal(t, 2, storedAudio)
			var providerAudio int
			for _, msg := range prov.messages {
				if msg.Role != chat.MessageRoleUser {
					continue
				}
				assert.Contains(t, msg.Content, promptAudioNote)
				for _, part := range msg.MultiContent {
					if part.Document != nil {
						providerAudio++
					}
				}
			}
			if supported {
				assert.Equal(t, 2, providerAudio)
			} else {
				assert.Zero(t, providerAudio)
			}
			text, found := c.Lookup(promptAudioNote)
			require.True(t, found)
			assert.Equal(t, "cached text-only answer", text)
		})
	}
}

func TestAudioCapabilityAndColdLoadReplay(t *testing.T) {
	t.Parallel()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	a := clientMCPAgent(t)
	a.team = nil
	a.agentSource = config.NewBytesSource("test", nil)
	a.sessionStore = store
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	response, err := a.Initialize(t.Context(), acpsdk.InitializeRequest{})
	require.NoError(t, err)
	assert.True(t, response.AgentCapabilities.PromptCapabilities.Audio)
	saved := session.New(session.WithWorkingDir(t.TempDir()))
	payload := base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4})
	saved.AddMessage(a.buildUserMessage(t.Context(), saved.ID, []acpsdk.ContentBlock{acpsdk.AudioBlock(payload, "audio/pcm;rate=24000"), textResource("file:///transcript.txt", "audio/mpeg", "explicit text remains text")}))
	require.NoError(t, store.AddSession(t.Context(), saved))
	out := &captureWriter{}
	captureReplay(t, a, out)
	_, err = a.LoadSession(t.Context(), acpsdk.LoadSessionRequest{SessionId: acpsdk.SessionId(saved.ID), Cwd: saved.WorkingDir})
	require.NoError(t, err)
	updates := replayUpdates(t, out, saved.ID)
	require.NotEmpty(t, updates)
	require.NotNil(t, updates[0].UserMessageChunk)
	require.NotNil(t, updates[0].UserMessageChunk.Content.Audio)
	assert.Equal(t, payload, updates[0].UserMessageChunk.Content.Audio.Data)
	assert.Equal(t, "audio/pcm; rate=24000", updates[0].UserMessageChunk.Content.Audio.MimeType)
	assert.Equal(t, promptAudioNote, updates[1].UserMessageChunk.Content.Text.Text)
	assert.Contains(t, updates[2].UserMessageChunk.Content.Text.Text, "explicit text remains text")
}

func TestAudioReplayBoundsAndExclusions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		role  chat.MessageRole
		doc   chat.Document
		audio bool
	}{
		{"limit", chat.MessageRoleUser, chat.Document{MimeType: "audio/wav", Source: chat.DocumentSource{InlineData: make([]byte, maxReplayUpdateBytes/2)}}, true},
		{"over limit", chat.MessageRoleUser, chat.Document{MimeType: "audio/wav", Source: chat.DocumentSource{InlineData: make([]byte, maxReplayUpdateBytes/2+1)}}, false},
		{"assistant", chat.MessageRoleAssistant, chat.Document{MimeType: "audio/wav", Source: chat.DocumentSource{InlineData: []byte{1}}}, false},
		{"artifact", chat.MessageRoleUser, chat.Document{MimeType: "audio/wav", Source: chat.DocumentSource{ArtifactPath: "do-not-read.wav", InlineData: []byte{1}}}, false},
		{"invalid MIME", chat.MessageRoleUser, chat.Document{MimeType: "audio/wav; broken", Source: chat.DocumentSource{InlineData: []byte{1}}}, false},
		{"long MIME", chat.MessageRoleUser, chat.Document{MimeType: "audio/wav; secret=" + strings.Repeat("x", 300), Source: chat.DocumentSource{InlineData: []byte{1}}}, false},
		{"empty", chat.MessageRoleUser, chat.Document{MimeType: "audio/wav"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, s := newResumeFixture(t, t.TempDir())
			out := &captureWriter{}
			captureReplay(t, a, out)
			require.NoError(t, a.replayMessageContent(t.Context(), s.id, chat.Message{Role: tc.role, MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeDocument, Document: &tc.doc}}}))
			updates := replayUpdates(t, out, s.id)
			require.Len(t, updates, 1)
			var block acpsdk.ContentBlock
			if tc.role == chat.MessageRoleAssistant {
				require.NotNil(t, updates[0].AgentMessageChunk)
				block = updates[0].AgentMessageChunk.Content
			} else {
				require.NotNil(t, updates[0].UserMessageChunk)
				block = updates[0].UserMessageChunk.Content
			}
			if tc.audio {
				require.NotNil(t, block.Audio)
			} else {
				require.NotNil(t, block.Text)
				assert.Contains(t, block.Text.Text, "unavailable")
			}
			assert.LessOrEqual(t, len(out.lines()[0]), maxReplayUpdateBytes+256)
		})
	}
}

func TestPromptAudioWireToProvider(t *testing.T) {
	t.Parallel()
	prov := &audioTestProvider{contentTestProvider: contentTestProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "audio")}}, audio: true}
	rt, err := runtime.New(t.Context(), team.New(team.WithAgents(agent.New("root", "test", agent.WithModel(prov)))), runtime.WithSessionCompaction(false))
	require.NoError(t, err)
	defer func() { require.NoError(t, rt.Close()) }()
	a, _, _ := newPromptTestAgent(t, rt)
	input, send := io.Pipe()
	receive, output := io.Pipe()
	conn := a.NewConnection(output, input)
	t.Cleanup(func() { _ = send.Close(); _ = receive.Close(); <-conn.Done() })
	payload := base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 255})
	request := promptRequest("")
	request.Prompt = []acpsdk.ContentBlock{acpsdk.AudioBlock(payload, "audio/wav")}
	require.NoError(t, json.NewEncoder(send).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "session/prompt", "params": request}))
	decoder := json.NewDecoder(receive)
	for {
		var response struct {
			ID     *int                   `json:"id"`
			Method string                 `json:"method"`
			Result *acpsdk.PromptResponse `json:"result"`
			Error  *acpsdk.RequestError   `json:"error"`
		}
		require.NoError(t, decoder.Decode(&response))
		if response.ID == nil {
			continue
		}
		require.Nil(t, response.Error)
		require.NotNil(t, response.Result)
		assert.Equal(t, acpsdk.StopReasonEndTurn, response.Result.StopReason)
		break
	}
	require.Equal(t, 1, prov.calls)
	var found bool
	for _, msg := range prov.messages {
		for _, part := range msg.MultiContent {
			if part.Document != nil {
				found = true
				assert.Equal(t, payload, base64.StdEncoding.EncodeToString(part.Document.Source.InlineData))
			}
		}
	}
	assert.True(t, found)
}
