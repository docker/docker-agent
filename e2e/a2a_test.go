package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	a2aserver "github.com/docker/docker-agent/pkg/a2a"
	"github.com/docker/docker-agent/pkg/config"
)

// a2aResponse is a simplified representation of a JSON-RPC response
// that only captures the fields we care about in tests.
type a2aResponse struct {
	Jsonrpc string `json:"jsonrpc"`
	ID      string `json:"id"`
	Error   any    `json:"error,omitempty"`

	// Result holds the task with its artifacts.
	Result *struct {
		Artifacts []struct {
			Parts []struct {
				Kind string `json:"kind"`
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"artifacts"`
	} `json:"result,omitempty"`
}

// textParts returns all text parts across every artifact in the response.
func (r *a2aResponse) textParts() []string {
	if r.Result == nil {
		return nil
	}
	var texts []string
	for _, a := range r.Result.Artifacts {
		for _, p := range a.Parts {
			if p.Kind == "text" {
				texts = append(texts, p.Text)
			}
		}
	}
	return texts
}

func TestA2AServer_AgentCard(t *testing.T) {
	t.Parallel()

	_, runConfig := startRecordingAIProxy(t)
	agentCard := startA2AServer(t, "testdata/basic.yaml", runConfig)

	assert.Equal(t, "basic", agentCard.Name)
	assert.NotEmpty(t, agentCard.Description)
	assert.Equal(t, a2a.TransportProtocolJSONRPC, agentCard.PreferredTransport)
	assert.Contains(t, agentCard.URL, "/invoke")
	assert.True(t, agentCard.Capabilities.Streaming)
	assert.NotEmpty(t, agentCard.Version)
}

func TestA2AServer_Invoke(t *testing.T) {
	t.Parallel()

	_, runConfig := startRecordingAIProxy(t)
	agentCard := startA2AServer(t, "testdata/basic.yaml", runConfig)

	resp := sendA2AMessage(t, agentCard.URL, "test-request-1", "msg-1", "What is 2+2? Answer with just the number.")

	assert.Equal(t, "2.0", resp.Jsonrpc)
	assert.Equal(t, "test-request-1", resp.ID)
	assert.Nil(t, resp.Error)
	require.NotNil(t, resp.Result)

	texts := resp.textParts()
	require.NotEmpty(t, texts)
	for _, text := range texts {
		assert.Equal(t, "4", text)
	}
}

func TestA2AServer_MultipleRequests(t *testing.T) {
	t.Parallel()

	_, runConfig := startRecordingAIProxy(t)
	agentCard := startA2AServer(t, "testdata/basic.yaml", runConfig)

	messages := []string{
		"Say 'hello' in one word.",
		"Say 'goodbye' in one word.",
	}

	for i, message := range messages {
		t.Run(fmt.Sprintf("request_%d", i), func(t *testing.T) {
			requestID := fmt.Sprintf("test-request-%d", i)
			msgID := fmt.Sprintf("msg-%d", i)

			resp := sendA2AMessage(t, agentCard.URL, requestID, msgID, message)

			assert.Equal(t, requestID, resp.ID)
			assert.Nil(t, resp.Error)
			assert.NotNil(t, resp.Result)
		})
	}
}

func TestA2AServer_MultiAgent(t *testing.T) {
	t.Parallel()

	_, runConfig := startRecordingAIProxy(t)
	agentCard := startA2AServer(t, "testdata/multi.yaml", runConfig)

	resp := sendA2AMessage(t, agentCard.URL, "test-multi-1", "msg-multi-1", "Say hello.")

	assert.Equal(t, "test-multi-1", resp.ID)
	assert.Nil(t, resp.Error)
	require.NotNil(t, resp.Result)

	texts := resp.textParts()
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[len(texts)-1], "Hello")
}

// sendA2AMessage sends a message/send JSON-RPC request and returns the parsed response.
func sendA2AMessage(t *testing.T, url, requestID, messageID, text string) a2aResponse {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"method":  "message/send",
		"params": map[string]any{
			"message": map[string]any{
				"messageId": messageID,
				"role":      "user",
				"parts":     []map[string]any{{"kind": "text", "text": text}},
			},
		},
	})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var parsed a2aResponse
	require.NoError(t, json.Unmarshal(data, &parsed))

	return parsed
}

func startA2AServer(t *testing.T, agentFile string, runConfig *config.RuntimeConfig) a2a.AgentCard {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", ":0")
	require.NoError(t, err)

	go func() {
		_ = a2aserver.Run(t.Context(), agentFile, "root", filepath.Join(t.TempDir(), "session.db"), runConfig, ln)
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	serverURL := fmt.Sprintf("http://localhost:%d", port)

	cardReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, serverURL+a2asrv.WellKnownAgentCardPath, http.NoBody)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(cardReq)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var agentCard a2a.AgentCard
	err = json.NewDecoder(resp.Body).Decode(&agentCard)
	require.NoError(t, err)

	return agentCard
}
