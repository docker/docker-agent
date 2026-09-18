package mcp

import (
	"context"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestSessionClientRequestContextUsesSoleInflightCall(t *testing.T) {
	t.Parallel()

	client := &sessionClient{}
	ctx := tools.WithHandlerScope(t.Context(), tools.HandlerScope{})
	id := client.registerCallContext(ctx)
	defer client.unregisterCallContext(id)

	assert.True(t, tools.HasHandlerScope(client.requestContext(t.Context())))
}

func TestSessionClientRequestContextRejectsAmbiguousInflightCalls(t *testing.T) {
	t.Parallel()

	client := &sessionClient{}
	ctx := tools.WithHandlerScope(t.Context(), tools.HandlerScope{})
	first := client.registerCallContext(ctx)
	second := client.registerCallContext(ctx)
	defer client.unregisterCallContext(first)
	defer client.unregisterCallContext(second)

	assert.False(t, tools.HasHandlerScope(client.requestContext(ctx)))
}

// applySamplingHandlerOpts makes for each combination of the two sampling
// handler fields. The reviewer flagged a reconnect race: if Initialize ran
// before configureToolsetHandlers had wired up the handlers, the old
// implementation registered neither CreateMessage* callback with the SDK and
// the next sampling/createMessage request from the server failed with no
// handler. The helper closes that race by registering the with-tools callback
// whenever it might ever be needed — including when both fields are still nil
// — while preserving the basic-only path for callers that explicitly chose it.
func TestApplySamplingHandlerOpts_RegistrationMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		setBasic          bool
		setWithTools      bool
		wantWithTools     bool
		wantBasic         bool
		registrationNotes string
	}{
		{
			name:              "neither handler set: still register with-tools",
			wantWithTools:     true,
			registrationNotes: "reconnect race guard — late SetSamplingWithToolsHandler must take effect",
		},
		{
			name:              "only with-tools handler set",
			setWithTools:      true,
			wantWithTools:     true,
			registrationNotes: "modern path",
		},
		{
			name:              "only basic handler set",
			setBasic:          true,
			wantBasic:         true,
			registrationNotes: "legacy path — caller demonstrated they only want basic sampling",
		},
		{
			name:              "both handlers set: prefer with-tools",
			setBasic:          true,
			setWithTools:      true,
			wantWithTools:     true,
			registrationNotes: "with-tools is a superset; SDK panics if both callbacks are registered",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var c sessionClient
			if tc.setBasic {
				c.SetSamplingHandler(func(context.Context, *gomcp.CreateMessageParams) (*gomcp.CreateMessageResult, error) {
					return &gomcp.CreateMessageResult{}, nil
				})
			}
			if tc.setWithTools {
				c.SetSamplingWithToolsHandler(func(context.Context, *gomcp.CreateMessageWithToolsParams) (*gomcp.CreateMessageWithToolsResult, error) {
					return &gomcp.CreateMessageWithToolsResult{}, nil
				})
			}

			opts := &gomcp.ClientOptions{}
			c.applySamplingHandlerOpts(opts)

			assert.Equalf(t, tc.wantWithTools, opts.CreateMessageWithToolsHandler != nil,
				"CreateMessageWithToolsHandler registration mismatch (%s)", tc.registrationNotes)
			assert.Equalf(t, tc.wantBasic, opts.CreateMessageHandler != nil,
				"CreateMessageHandler registration mismatch (%s)", tc.registrationNotes)
			// The SDK panics if both are populated; pin that we never end up there.
			assert.Falsef(t, opts.CreateMessageHandler != nil && opts.CreateMessageWithToolsHandler != nil,
				"both CreateMessage* handlers registered — SDK would panic")
		})
	}
}

// TestHandleSamplingWithToolsRequest_LateSetterTakesEffect proves the
// lazy-read behaviour the registration guard relies on: a
// SetSamplingWithToolsHandler call that lands AFTER applySamplingHandlerOpts
// has already wired the callback into the SDK still has its handler invoked
// on the next inbound sampling/createMessage request. This is what makes the
// race-free reconnect path safe — Initialize doesn't need to re-run for late
// handler registration to become effective.
func TestHandleSamplingWithToolsRequest_LateSetterTakesEffect(t *testing.T) {
	t.Parallel()

	var c sessionClient

	// Mimic the Initialize-then-handler-registration order: helper runs
	// first (registering the with-tools callback against a still-nil
	// handler field), then SetSamplingWithToolsHandler lands.
	opts := &gomcp.ClientOptions{}
	c.applySamplingHandlerOpts(opts)
	require.NotNil(t, opts.CreateMessageWithToolsHandler, "with-tools callback must be wired even with nil handler field")

	// Sanity-check the pre-registration behaviour: an inbound request with
	// no handler yet must surface a clean error, not a panic or silent drop.
	_, err := c.handleSamplingWithToolsRequest(t.Context(), &gomcp.CreateMessageWithToolsRequest{Params: &gomcp.CreateMessageWithToolsParams{}})
	require.Error(t, err, "handler must error when no SamplingWithToolsHandler is set yet")

	// Late registration — the simulated post-reconnect wiring step.
	called := false
	c.SetSamplingWithToolsHandler(func(context.Context, *gomcp.CreateMessageWithToolsParams) (*gomcp.CreateMessageWithToolsResult, error) {
		called = true
		return &gomcp.CreateMessageWithToolsResult{Model: "late-bound"}, nil
	})

	result, err := c.handleSamplingWithToolsRequest(t.Context(), &gomcp.CreateMessageWithToolsRequest{Params: &gomcp.CreateMessageWithToolsParams{}})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, called, "late SetSamplingWithToolsHandler must take effect without re-init")
	assert.Equal(t, "late-bound", result.Model)
}
