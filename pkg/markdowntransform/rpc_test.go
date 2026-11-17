package markdowntransform

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rpcPeer connects an rpcClient to an in-memory server.
func rpcPeer(t *testing.T, serve func(request map[string]any) []string) *rpcClient {
	t.Helper()
	requestReader, requestWriter := io.Pipe()
	lines := make(chan []byte)
	t.Cleanup(func() { _ = requestWriter.Close() })
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(requestReader)
		for scanner.Scan() {
			var request map[string]any
			if json.Unmarshal(scanner.Bytes(), &request) != nil {
				return
			}
			for _, line := range serve(request) {
				lines <- []byte(line)
			}
		}
	}()
	return &rpcClient{w: requestWriter, lines: lines}
}

func TestRPCClientCall(t *testing.T) {
	t.Parallel()
	client := rpcPeer(t, func(request map[string]any) []string {
		assert.Equal(t, "2.0", request["jsonrpc"])
		assert.Equal(t, "echo", request["method"])
		return []string{`{"jsonrpc":"2.0","method":"notice"}`, `{"jsonrpc":"2.0","id":99,"result":"stale"}`, `{"jsonrpc":"2.0","id":1,"result":{"value":"ok"}}`}
	})
	var result struct{ Value string }
	require.NoError(t, client.call(t.Context(), "echo", map[string]string{"a": "b"}, &result))
	assert.Equal(t, "ok", result.Value)
}

func TestRPCClientErrors(t *testing.T) {
	t.Parallel()
	reply := rpcPeer(t, func(map[string]any) []string {
		return []string{`{"jsonrpc":"2.0","id":1,"error":{"code":7,"message":"no"}}`}
	})
	var replyErr *rpcError
	require.ErrorAs(t, reply.call(t.Context(), "m", nil, &struct{}{}), &replyErr)
	assert.Equal(t, 7, replyErr.Code)

	invalid := rpcPeer(t, func(map[string]any) []string { return []string{`{"jsonrpc":"2.0","id":1,"result":1}`} })
	require.ErrorAs(t, invalid.call(t.Context(), "m", nil, &struct{ Value string }{}), &replyErr)

	garbage := rpcPeer(t, func(map[string]any) []string { return []string{"not json"} })
	err := garbage.call(t.Context(), "m", nil, &struct{}{})
	require.Error(t, err)
	assert.NotErrorAs(t, err, &replyErr)

	closed := make(chan []byte)
	close(closed)
	require.ErrorIs(t, (&rpcClient{w: io.Discard, lines: closed}).call(t.Context(), "m", nil, &struct{}{}), errStreamClosed)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	silent := rpcPeer(t, func(map[string]any) []string { return nil })
	require.ErrorIs(t, silent.call(ctx, "m", nil, &struct{}{}), context.DeadlineExceeded)
}
