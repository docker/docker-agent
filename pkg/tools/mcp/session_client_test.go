package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestElicitationContextUsesOwningCallCancellation(t *testing.T) {
	t.Parallel()
	for _, end := range []string{"cancel", "unregister", "peer"} {
		t.Run(end, func(t *testing.T) {
			client := &sessionClient{}
			owner, cancelOwner := context.WithCancel(tools.WithHandlerScope(t.Context(), tools.HandlerScope{}))
			defer cancelOwner()
			peer, cancelPeer := context.WithCancel(t.Context())
			defer cancelPeer()
			id := client.registerCallContext(owner)
			defer client.unregisterCallContext(id)
			ctx, release := client.elicitationContext(peer)
			defer release()
			require.True(t, tools.HasHandlerScope(ctx))
			switch end {
			case "cancel":
				cancelOwner()
			case "unregister":
				client.unregisterCallContext(id)
			case "peer":
				cancelPeer()
			}
			<-ctx.Done()
			require.ErrorIs(t, ctx.Err(), context.Canceled)
		})
	}
}

func TestElicitationContextAmbiguityDoesNotSelectAnOwner(t *testing.T) {
	t.Parallel()
	client := &sessionClient{}
	owner, cancel := context.WithCancel(tools.WithHandlerScope(t.Context(), tools.HandlerScope{}))
	defer cancel()
	first := client.registerCallContext(owner)
	defer client.unregisterCallContext(first)
	second := client.registerCallContext(owner)
	defer client.unregisterCallContext(second)
	ctx, release := client.elicitationContext(owner)
	defer release()
	assert.False(t, tools.HasHandlerScope(ctx))
}
