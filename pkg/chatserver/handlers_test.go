package chatserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestServer builds a server with a fake policy for tests that don't run
// the agent loop. Handlers that touch s.team will panic — those code paths
// are exercised by integration tests, not here.
func newTestServer(exposed ...string) (*server, *echo.Echo) {
	if len(exposed) == 0 {
		exposed = []string{"root"}
	}
	srv := &server{
		policy:            agentPolicy{exposed: exposed, fallback: exposed[0]},
		conversationLocks: newConversationLockSet(),
	}
	e := echo.New()
	return srv, e
}

func TestHandleModels(t *testing.T) {
	srv, e := newTestServer("root", "reviewer")

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", http.NoBody)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	require.NoError(t, srv.handleModels(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var got ModelsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "list", got.Object)
	require.Len(t, got.Data, 2)

	ids := []string{got.Data[0].ID, got.Data[1].ID}
	assert.ElementsMatch(t, []string{"root", "reviewer"}, ids)
	for _, m := range got.Data {
		assert.Equal(t, "docker-agent", m.OwnedBy)
		// openai.Model carries a typed `Object constant.Model` field that
		// always serialises to "model". Ensure the wire shape is stable.
		assert.Equal(t, openai.Model{}.Object.Default(), m.Object)
	}
}

func TestHandleChatCompletions_RejectsBadJSON(t *testing.T) {
	srv, e := newTestServer()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	require.NoError(t, srv.handleChatCompletions(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid_request_error")
}

func TestHandleChatCompletions_RejectsEmptyMessages(t *testing.T) {
	srv, e := newTestServer()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	require.NoError(t, srv.handleChatCompletions(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var got ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "invalid_request_error", got.Error.Type)
	assert.Contains(t, got.Error.Message, "at least one message")
}

func TestHandleChatCompletions_RejectsHistoryWithoutUser(t *testing.T) {
	srv, e := newTestServer()

	body := `{"messages":[{"role":"system","content":"be helpful"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	require.NoError(t, srv.handleChatCompletions(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "no user message")
}

func TestWriteError_ShapeAndType(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		message  string
		wantType string
	}{
		{"client error", http.StatusBadRequest, "bad input", "invalid_request_error"},
		{"server error", http.StatusInternalServerError, "boom", "internal_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)

			require.NoError(t, writeError(c, tc.status, tc.message))
			assert.Equal(t, tc.status, rec.Code)

			var got ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			assert.Equal(t, tc.message, got.Error.Message)
			assert.Equal(t, tc.wantType, got.Error.Type)
		})
	}
}
