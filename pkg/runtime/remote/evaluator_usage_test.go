package remote

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestClientDecodesEvaluationUsage(t *testing.T) {
	t.Parallel()
	sess := session.New()
	evaluation := &session.Evaluation{ID: "request", Evaluator: "safety", AgentName: "root", Model: "jev", Usage: &chat.Usage{InputTokens: 12}, Cost: new(0.25), CreatedAt: time.Now().Format(time.RFC3339)}
	event := &agentruntime.EvaluationUsageEvent{Type: "evaluation_usage", SessionID: sess.ID, AgentContext: agentruntime.AgentContext{AgentName: "root", Timestamp: time.Now()}, Evaluation: evaluation}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		payload, err := json.Marshal(event)
		assert.NoError(t, err)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)
	stream, err := client.StreamSessionEvents(t.Context(), sess.ID)
	require.NoError(t, err)
	var received []*agentruntime.EvaluationUsageEvent
	for e := range stream {
		if usage, ok := e.(*agentruntime.EvaluationUsageEvent); ok {
			received = append(received, usage)
		}
	}
	require.Len(t, received, 1)
	assert.Equal(t, event.Evaluation, received[0].Evaluation)
	assert.Equal(t, event.SessionID, received[0].SessionID)
	assert.Equal(t, event.AgentName, received[0].AgentName)
	assert.True(t, event.Timestamp.Equal(received[0].Timestamp))
}
