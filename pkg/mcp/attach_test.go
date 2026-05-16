package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttachServer_RequiresArgs(t *testing.T) {
	ctx := t.Context()

	_, err := AttachServer(ctx, "", "session-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "addr and sessionID")

	_, err = AttachServer(ctx, "http://127.0.0.1:1234", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "addr and sessionID")
}

func TestAttachServer_BuildsWithValidArgs(t *testing.T) {
	ctx := t.Context()

	srv, err := AttachServer(ctx, "http://127.0.0.1:1234", "session-1")
	require.NoError(t, err)
	assert.NotNil(t, srv)
}
