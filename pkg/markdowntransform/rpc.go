package markdowntransform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// errStreamClosed reports that the peer stopped producing lines.
var errStreamClosed = errors.New("stream closed")

// rpcError is a well-formed JSON-RPC error reply; the stream stays usable.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message) }

// rpcClient speaks newline-delimited JSON-RPC 2.0 over a line stream. Calls
// must not overlap; replies to other IDs are skipped.
type rpcClient struct {
	w      io.Writer
	lines  <-chan []byte
	nextID uint64
}

func (c *rpcClient) call(ctx context.Context, method string, params, result any) error {
	c.nextID++
	id := c.nextID
	request, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      uint64 `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", id, method, params})
	if err != nil {
		return err
	}

	written := make(chan error, 1)
	go func() {
		_, err := c.w.Write(append(request, '\n'))
		written <- err
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-written:
			if err != nil {
				return fmt.Errorf("write request: %w", err)
			}
		case line, ok := <-c.lines:
			if !ok {
				return errStreamClosed
			}
			var response struct {
				ID     uint64          `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *rpcError       `json:"error"`
			}
			if err := json.Unmarshal(line, &response); err != nil {
				return fmt.Errorf("invalid response: %w", err)
			}
			if response.ID != id {
				continue
			}
			if response.Error != nil {
				return response.Error
			}
			if err := json.Unmarshal(response.Result, result); err != nil {
				return &rpcError{Code: -32603, Message: "invalid result: " + err.Error()}
			}
			return nil
		}
	}
}
