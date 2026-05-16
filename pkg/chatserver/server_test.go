package chatserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestBuildSession_RequiresUserMessage(t *testing.T) {
	tests := []struct {
		name     string
		messages []ChatCompletionMessage
		wantNil  bool
	}{
		{
			name:    "empty list",
			wantNil: true,
		},
		{
			name: "only system messages",
			messages: []ChatCompletionMessage{
				{Role: "system", Content: "be helpful"},
			},
			wantNil: true,
		},
		{
			name: "blank user message is ignored",
			messages: []ChatCompletionMessage{
				{Role: "user", Content: "   "},
			},
			wantNil: true,
		},
		{
			name: "valid user message",
			messages: []ChatCompletionMessage{
				{Role: "user", Content: "hello"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sess := buildSession(tc.messages)
			if tc.wantNil {
				assert.Nil(t, sess)
				return
			}
			require.NotNil(t, sess)
			assert.True(t, sess.ToolsApproved)
			assert.True(t, sess.NonInteractive)
		})
	}
}

func TestBuildSession_PreservesHistory(t *testing.T) {
	sess := buildSession([]ChatCompletionMessage{
		{Role: "system", Content: "you are a docker agent"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
		{Role: "user", Content: "how are you?"},
	})
	require.NotNil(t, sess)

	// GetAllMessages omits system messages.
	all := sess.GetAllMessages()
	require.Len(t, all, 3)

	roles := make([]chat.MessageRole, len(all))
	for i, m := range all {
		roles[i] = m.Message.Role
	}
	assert.Equal(t, []chat.MessageRole{
		chat.MessageRoleUser,
		chat.MessageRoleAssistant,
		chat.MessageRoleUser,
	}, roles)

	assert.Equal(t, "how are you?", sess.GetLastUserMessageContent())
	assert.Equal(t, "hi there", sess.GetLastAssistantMessageContent())
}

func TestBuildSession_PreservesToolMessage(t *testing.T) {
	sess := buildSession([]ChatCompletionMessage{
		{Role: "user", Content: "compute 2+2"},
		{Role: "assistant", Content: ""}, // dropped: empty content
		{Role: "tool", Content: "4", ToolCallID: "call_1"},
	})
	require.NotNil(t, sess)

	all := sess.GetAllMessages()
	require.Len(t, all, 2)

	last := all[len(all)-1].Message
	assert.Equal(t, chat.MessageRoleTool, last.Role)
	assert.Equal(t, "4", last.Content)
	assert.Equal(t, "call_1", last.ToolCallID)
}

func TestBuildSession_UnknownRoleTreatedAsUser(t *testing.T) {
	sess := buildSession([]ChatCompletionMessage{
		{Role: "developer", Content: "do this"},
	})
	require.NotNil(t, sess)

	all := sess.GetAllMessages()
	require.Len(t, all, 1)
	assert.Equal(t, chat.MessageRoleUser, all[0].Message.Role)
	assert.Equal(t, "do this", all[0].Message.Content)
}

func TestAgentPolicy_Pick(t *testing.T) {
	p := agentPolicy{exposed: []string{"root", "reviewer"}, fallback: "root"}

	assert.Equal(t, "reviewer", p.pick("reviewer"))
	assert.Equal(t, "root", p.pick("root"))
	assert.Equal(t, "root", p.pick(""), "empty model falls back")
	assert.Equal(t, "root", p.pick("gpt-4"), "unknown model falls back")
}

func TestErrTypeFor(t *testing.T) {
	assert.Equal(t, "invalid_request_error", errTypeFor(400))
	assert.Equal(t, "invalid_request_error", errTypeFor(404))
	assert.Equal(t, "internal_error", errTypeFor(500))
	assert.Equal(t, "internal_error", errTypeFor(502))
}

func TestNewRouter_CORSDisabledByDefault(t *testing.T) {
	srv, _ := newTestServer("root")
	r := newRouter(srv, Options{})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/v1/models", http.NoBody)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"),
		"no CORS header should be emitted when no origin is configured")
}

func TestNewRouter_CORSAllowsConfiguredOrigin(t *testing.T) {
	srv, _ := newTestServer("root")
	r := newRouter(srv, Options{CORSOrigin: "https://example.com"})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/v1/models", http.NoBody)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, "https://example.com", rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestCorsMiddlewareConfig(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		wantErr bool
	}{
		{name: "single literal", spec: "https://app.example.com"},
		{name: "comma list", spec: "https://a.example.com, https://b.example.com"},
		{name: "regex", spec: `~^https://[a-z]+\.example\.com$`},
		{name: "wildcard", spec: "*"},
		{name: "mixed", spec: `https://a.example.com,~^https://b\.example\.com$`},
		{name: "empty entries collapse", spec: ", , https://x.com,,"},

		{name: "missing scheme", spec: "app.example.com", wantErr: true},
		{name: "with path", spec: "https://example.com/api", wantErr: true},
		{name: "with query", spec: "https://example.com?x=1", wantErr: true},
		{name: "bad regex", spec: "~[", wantErr: true},
		{name: "all blanks", spec: ", , ,", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := corsMiddlewareConfig(tc.spec)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestNewRouter_CORSAllowList(t *testing.T) {
	srv, _ := newTestServer("root")
	r := newRouter(srv, Options{CORSOrigin: "https://a.example.com,https://b.example.com"})

	cases := []struct {
		origin string
		want   string // expected Access-Control-Allow-Origin
	}{
		{"https://a.example.com", "https://a.example.com"},
		{"https://b.example.com", "https://b.example.com"},
		{"https://evil.example.com", ""},
	}
	for _, tc := range cases {
		t.Run(tc.origin, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/v1/models", http.NoBody)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Access-Control-Request-Method", "GET")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Header().Get("Access-Control-Allow-Origin"))
		})
	}
}

func TestNewRouter_CORSRegex(t *testing.T) {
	srv, _ := newTestServer("root")
	r := newRouter(srv, Options{CORSOrigin: `~^https://[a-z]+\.example\.com$`})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/v1/models", http.NoBody)
	req.Header.Set("Origin", "https://staging.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, "https://staging.example.com", rec.Header().Get("Access-Control-Allow-Origin"))

	// A non-matching origin must not get the header.
	req2 := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/v1/models", http.NoBody)
	req2.Header.Set("Origin", "https://evil.attacker.com")
	req2.Header.Set("Access-Control-Request-Method", "GET")
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	assert.Empty(t, rec2.Header().Get("Access-Control-Allow-Origin"))
}

func TestBearerAuthMiddleware(t *testing.T) {
	cases := []struct {
		name       string
		header     string
		wantStatus int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic abc", http.StatusUnauthorized},
		{"wrong token", "Bearer wrong", http.StatusUnauthorized},
		{"correct token", "Bearer secret", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServer("root")
			r := newRouter(srv, Options{APIKey: "secret"})

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", http.NoBody)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}
}

func TestHandleChatCompletions_RejectsConcurrentSameConversation(t *testing.T) {
	srv, _ := newTestServer("root")
	r := newRouter(srv, Options{})

	// Pre-acquire the conversation lock to simulate an in-flight
	// request. The next request with the same id must get 409.
	require.True(t, srv.conversationLocks.tryAcquire("conv-x"))
	defer srv.conversationLocks.release("conv-x")

	body := `{"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Conversation-Id", "conv-x")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "another request is already in flight")
}

func TestBearerAuthMiddleware_AllowsCORSPreflight(t *testing.T) {
	// CORS preflight must succeed without an Authorization header.
	srv, _ := newTestServer("root")
	r := newRouter(srv, Options{APIKey: "secret", CORSOrigin: "https://example.com"})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/v1/models", http.NoBody)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.NotEqual(t, http.StatusUnauthorized, rec.Code)
}

func TestNewRouter_RejectsOversizedBody(t *testing.T) {
	srv, _ := newTestServer("root")
	r := newRouter(srv, Options{MaxRequestBytes: 16})

	body := strings.NewReader(`{"messages":[{"role":"user","content":"this body is far longer than sixteen bytes"}]}`)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestValidateSamplingParams(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	i := func(v int64) *int64 { return &v }

	cases := []struct {
		name    string
		req     ChatCompletionRequest
		wantErr string
	}{
		{name: "all empty"},
		{name: "valid", req: ChatCompletionRequest{
			Temperature: f(0.7), TopP: f(0.95), MaxTokens: i(256),
			Stop: StopSequences{"\n\n", "END"},
		}},
		{name: "temp negative", req: ChatCompletionRequest{Temperature: f(-0.1)}, wantErr: "temperature"},
		{name: "temp too high", req: ChatCompletionRequest{Temperature: f(2.5)}, wantErr: "temperature"},
		{name: "topp zero", req: ChatCompletionRequest{TopP: f(0)}, wantErr: "top_p"},
		{name: "topp too high", req: ChatCompletionRequest{TopP: f(1.5)}, wantErr: "top_p"},
		{name: "max_tokens zero", req: ChatCompletionRequest{MaxTokens: i(0)}, wantErr: "max_tokens"},
		{name: "empty stop", req: ChatCompletionRequest{Stop: StopSequences{""}}, wantErr: "stop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSamplingParams(&tc.req)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestChatCompletionMessage_UnmarshalContentString(t *testing.T) {
	var m ChatCompletionMessage
	require.NoError(t, json.Unmarshal([]byte(`{"role":"user","content":"hello"}`), &m))
	assert.Equal(t, "user", m.Role)
	assert.Equal(t, "hello", m.Content)
	assert.Empty(t, m.Parts)
}

func TestChatCompletionMessage_UnmarshalContentParts(t *testing.T) {
	var m ChatCompletionMessage
	input := `{
		"role":"user",
		"content":[
			{"type":"text","text":"What is in this picture?"},
			{"type":"image_url","image_url":{"url":"https://example.com/x.png","detail":"high"}}
		]
	}`
	require.NoError(t, json.Unmarshal([]byte(input), &m))
	assert.Equal(t, "user", m.Role)
	require.Len(t, m.Parts, 2)
	assert.Equal(t, "text", m.Parts[0].Type)
	assert.Equal(t, "image_url", m.Parts[1].Type)
	require.NotNil(t, m.Parts[1].ImageURL)
	assert.Equal(t, "https://example.com/x.png", m.Parts[1].ImageURL.URL)
	// Flat text is pre-computed for callers that don't care about parts.
	assert.Equal(t, "What is in this picture?", m.Content)
}

func TestChatCompletionMessage_RoundTripText(t *testing.T) {
	in := ChatCompletionMessage{Role: "assistant", Content: "hi there"}
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	assert.JSONEq(t, `{"role":"assistant","content":"hi there"}`, string(raw))
}

func TestChatCompletionMessage_RoundTripParts(t *testing.T) {
	in := ChatCompletionMessage{
		Role: "user",
		Parts: []ContentPart{
			{Type: "text", Text: "hi"},
			{Type: "image_url", ImageURL: &ContentImageURL{URL: "http://x/y"}},
		},
	}
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	assert.JSONEq(t, `{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"http://x/y"}}]}`, string(raw))
}

func TestBuildSession_AcceptsImageParts(t *testing.T) {
	sess := buildSession([]ChatCompletionMessage{{
		Role: "user",
		Parts: []ContentPart{
			{Type: "text", Text: "What is this?"},
			{Type: "image_url", ImageURL: &ContentImageURL{URL: "https://example.com/x.png"}},
		},
	}})
	require.NotNil(t, sess)

	all := sess.GetAllMessages()
	require.Len(t, all, 1)
	last := all[0].Message
	assert.Equal(t, chat.MessageRoleUser, last.Role)
	require.Len(t, last.MultiContent, 2)
	assert.Equal(t, chat.MessagePartTypeText, last.MultiContent[0].Type)
	assert.Equal(t, chat.MessagePartTypeImageURL, last.MultiContent[1].Type)
	require.NotNil(t, last.MultiContent[1].ImageURL)
	assert.Equal(t, "https://example.com/x.png", last.MultiContent[1].ImageURL.URL)
}

func TestStopSequences_UnmarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		json string
		want []string
		err  bool
	}{
		{"null", `null`, nil, false},
		{"single string", `"END"`, []string{"END"}, false},
		{"array", `["a", "b"]`, []string{"a", "b"}, false},
		{"empty array", `[]`, []string{}, false},
		{"number invalid", `42`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got StopSequences
			err := got.UnmarshalJSON([]byte(tc.json))
			if tc.err {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, []string(got))
		})
	}
}

func TestChatCompletionRequest_UnmarshalStreamOptions(t *testing.T) {
	var req ChatCompletionRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"stream": true,
		"stream_options": {"include_usage": true}
	}`), &req))
	require.NotNil(t, req.StreamOptions)
	assert.True(t, req.Stream)
	assert.True(t, req.StreamOptions.IncludeUsage)
}

func TestSSEStream_ToolCallDelta(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newSSEStream(rec, "chatcmpl-x", "root")
	s.send(ChatCompletionStreamDelta{ToolCalls: []ToolCallReference{{
		Index: 0,
		ID:    "call_1",
		Type:  "function",
		Function: ToolCallFunction{
			Name:      "search",
			Arguments: `{"q":"docker"}`,
		},
	}}}, "")

	body := rec.Body.String()
	assert.Contains(t, body, `"tool_calls":[`)
	assert.Contains(t, body, `"id":"call_1"`)
	assert.Contains(t, body, `"name":"search"`)
	assert.Contains(t, body, `"arguments":"{\"q\":\"docker\"}"`)
}

func TestSSEStream_SendUsage(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newSSEStream(rec, "chatcmpl-x", "root")
	s.send(ChatCompletionStreamDelta{}, "stop")
	s.sendUsage(&ChatCompletionUsage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12})
	s.done()

	body := rec.Body.String()
	assert.Contains(t, body, `"finish_reason":"stop"`)
	assert.Contains(t, body, `"choices":[]`)
	assert.Contains(t, body, `"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}`)
	assert.Contains(t, body, "data: [DONE]")
}

func TestSSEStream_SendError(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newSSEStream(rec, "chatcmpl-x", "root")
	s.sendError(errors.New("model exploded"))
	s.send(ChatCompletionStreamDelta{}, "error")
	s.done()

	body := rec.Body.String()
	// One error envelope.
	assert.Contains(t, body, `"error":{"message":"model exploded"`)
	// One terminating chunk with finish_reason=error (instead of stop).
	assert.Contains(t, body, `"finish_reason":"error"`)
	// And the OpenAI sentinel.
	assert.Contains(t, body, "data: [DONE]")
}

func TestRequestTimeoutMiddleware_AppliesDeadline(t *testing.T) {
	e := echo.New()
	e.Use(requestTimeoutMiddleware(5 * time.Millisecond))

	var gotErr error
	e.GET("/sleep", func(c echo.Context) error {
		select {
		case <-c.Request().Context().Done():
			gotErr = c.Request().Context().Err()
			return c.String(http.StatusOK, "ok")
		case <-time.After(time.Second):
			return c.String(http.StatusOK, "too slow")
		}
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/sleep", http.NoBody)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Error(t, gotErr)
	assert.ErrorIs(t, gotErr, context.DeadlineExceeded)
}
