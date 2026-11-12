package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/cmd/root"
	"github.com/docker/docker-agent/pkg/concurrent"
)

func TestServeChatConversationFailedTurnDoesNotAdvanceCache(t *testing.T) {
	modelServer := newRecordingChatCompletionsServer(t)

	agentFile := filepath.Join(t.TempDir(), "agent.yaml")
	agentYAML := fmt.Appendf(nil, `version: "9"

providers:
  e2e:
    api_type: openai_chatcompletions
    base_url: %s/v1

models:
  e2e-model:
    provider: e2e
    model: e2e-model
    max_tokens: 64

agents:
  root:
    model: e2e-model
    description: E2E chat server agent
    instruction: Reply concisely.
`, modelServer.URL())
	require.NoError(t, os.WriteFile(agentFile, agentYAML, 0o644))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	os.Unsetenv("DOCKER_CLI_PLUGIN_ORIGINAL_CLI_COMMAND")
	t.Setenv("DOCKER_AGENT_MODELS_GATEWAY", "")
	t.Setenv("CAGENT_MODELS_GATEWAY", "")
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	var stdout, stderr concurrent.Buffer
	done := make(chan struct{})
	var runErr error
	args := []string{
		"--cache-dir", filepath.Join(t.TempDir(), "cache"),
		"--config-dir", filepath.Join(t.TempDir(), "config"),
		"--data-dir", filepath.Join(t.TempDir(), "data"),
		"serve", "chat",
		"--listen", "127.0.0.1:0",
		"--conversations-max", "10",
		"--request-timeout", "30s",
		agentFile,
	}
	go func() {
		defer close(done)
		runErr = root.Execute(ctx, nil, &stdout, &stderr, args...)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
			if runErr != nil && !strings.Contains(runErr.Error(), "use of closed network connection") {
				assert.NoError(t, runErr, "stdout: %s\nstderr: %s", stdout.String(), stderr.String())
			}
		case <-time.After(5 * time.Second):
			t.Error("chat server did not stop")
		}
	})
	readyCtx, readyCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer readyCancel()
	baseURL, err := waitForChatServer(readyCtx, &stdout, done)
	require.NoError(t, err, "stdout: %s\nstderr: %s", stdout.String(), stderr.String())

	convID := "e2e-failed-turn"
	postChatCompletion(t, baseURL, convID, http.StatusOK, "first")
	responseBody := postChatCompletion(t, baseURL, convID, http.StatusInternalServerError, "please fail")
	require.Contains(t, string(responseBody), "forced failure")
	postChatCompletion(t, baseURL, convID, http.StatusOK, "after failure")

	requests := modelServer.requests()
	require.Len(t, requests, 3)
	assert.Equal(t, []string{"first"}, requests[0].userMessages())
	assert.Equal(t, []string{"first", "please fail"}, requests[1].userMessages())

	// This is the end-to-end assertion for #2890: the failed "please fail"
	// turn must not have been committed to the X-Conversation-Id cache, so the
	// following successful turn resumes from the last successful state.
	assert.Equal(t, []string{"first", "after failure"}, requests[2].userMessages())
}

type recordingChatCompletionsServer struct {
	server *httptest.Server
	mu     sync.Mutex
	reqs   []recordedChatCompletionRequest
}

type recordedChatCompletionRequest struct {
	Messages []recordedChatMessage `json:"messages"`
}

type recordedChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func newRecordingChatCompletionsServer(t *testing.T) *recordingChatCompletionsServer {
	t.Helper()

	rec := &recordingChatCompletionsServer{}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}

		var req recordedChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		rec.mu.Lock()
		rec.reqs = append(rec.reqs, req)
		rec.mu.Unlock()

		if lastUserMessage(req.Messages) == "please fail" {
			w.Header().Set("Content-Type", "application/json")
			// Fail without SDK/runtime retries; cache rollback must not depend on a timeout.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"forced failure","type":"invalid_request_error"}}`)
			return
		}

		writeChatCompletionsStream(w, "ok: "+lastUserMessage(req.Messages))
	}))
	t.Cleanup(rec.server.Close)
	return rec
}

func (s *recordingChatCompletionsServer) requests() []recordedChatCompletionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

func (s *recordingChatCompletionsServer) URL() string {
	return s.server.URL
}

func (r recordedChatCompletionRequest) userMessages() []string {
	var out []string
	for _, msg := range r.Messages {
		if msg.Role == "user" {
			out = append(out, messageContentText(msg.Content))
		}
	}
	return out
}

func lastUserMessage(messages []recordedChatMessage) string {
	for _, message := range slices.Backward(messages) {
		if message.Role == "user" {
			return messageContentText(message.Content)
		}
	}
	return ""
}

func messageContentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			m, ok := part.(map[string]any)
			if !ok || m["type"] != "text" {
				continue
			}
			if text, ok := m["text"].(string); ok {
				b.WriteString(text)
			}
		}
		return b.String()
	default:
		return fmt.Sprint(v)
	}
}

func writeChatCompletionsStream(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	writeSSEData(w, map[string]any{
		"id":      "chatcmpl-e2e",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   "e2e-model",
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{"role": "assistant", "content": content},
		}},
	})
	writeSSEData(w, map[string]any{
		"id":      "chatcmpl-e2e",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   "e2e-model",
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
	})
	writeSSEData(w, map[string]any{
		"id":      "chatcmpl-e2e",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   "e2e-model",
		"choices": []map[string]any{},
		"usage": map[string]any{
			"prompt_tokens":     1,
			"completion_tokens": 1,
			"total_tokens":      2,
		},
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeSSEData(w http.ResponseWriter, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func waitForChatServer(ctx context.Context, stdout *concurrent.Buffer, done <-chan struct{}) (string, error) {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-done:
			return "", errors.New("chat server stopped before becoming ready")
		case <-ticker.C:
		}
		for line := range strings.SplitSeq(stdout.String(), "\n") {
			addr, ok := strings.CutPrefix(line, "Listening on ")
			if !ok {
				continue
			}
			baseURL := "http://" + strings.TrimSpace(addr)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", http.NoBody)
			if err != nil {
				return "", err
			}
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if err == nil && resp.StatusCode == http.StatusOK {
				return baseURL, nil
			}
		}
	}
}

func TestWaitForChatServerStopped(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	close(done)
	_, err := waitForChatServer(t.Context(), &concurrent.Buffer{}, done)
	require.ErrorContains(t, err, "stopped before becoming ready")
}

func TestWaitForChatServerCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := waitForChatServer(ctx, &concurrent.Buffer{}, make(chan struct{}))
	require.ErrorIs(t, err, context.Canceled)
}

func postChatCompletion(t *testing.T, baseURL, conversationID string, expectedStatus int, userMessage string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "root",
		"messages": []map[string]string{{
			"role":    "user",
			"content": userMessage,
		}},
	})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Conversation-Id", conversationID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, expectedStatus, resp.StatusCode, "response body: %s", string(responseBody))
	return responseBody
}
