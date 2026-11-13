package remote

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
)

func TestClientOversizedEditContextDoesNotDisconnectStream(t *testing.T) {
	t.Parallel()
	context := strings.Repeat("x", 17<<20) + "\n"
	meta, err := filesystem.NewEditFileMeta("file.txt", context+"old\n", context+"new\n")
	require.NoError(t, err)
	event := agentruntime.ToolCallResponse("edit", tools.Tool{Name: "edit_file"}, &tools.ToolCallResult{Output: "edited", Meta: meta}, "edited", "root")
	data, err := json.Marshal(event)
	require.NoError(t, err)
	require.Less(t, len(data)+len("data: \n\n"), maxSSELineBytes)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: {\"type\":\"warning\",\"message\":\"after diff\"}\n\ndata: {\"type\":\"stream_stopped\"}\n\n", data)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)
	stream, err := client.RunAgent(t.Context(), "session", "agent.yaml", nil, "")
	require.NoError(t, err)
	var received []agentruntime.Event
	for event := range stream {
		received = append(received, event)
	}
	require.Len(t, received, 3)
	response, ok := received[0].(*agentruntime.ToolCallResponseEvent)
	require.True(t, ok)
	decoded, ok := filesystem.DecodeEditFileMeta(response.Result.Meta)
	require.True(t, ok)
	assert.True(t, decoded.Truncated)
	require.NotEmpty(t, decoded.Hunks)
	assert.IsType(t, &agentruntime.WarningEvent{}, received[1])
	assert.IsType(t, &agentruntime.StreamStoppedEvent{}, received[2])
}
