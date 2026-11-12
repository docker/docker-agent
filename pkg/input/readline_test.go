package input

import (
	"context"
	"io"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, input, want string
		wantErr           error
	}{
		{name: "line", input: "hello\n", want: "hello\n"},
		{name: "first line", input: "first\nsecond\n", want: "first\n"},
		{name: "empty", wantErr: io.EOF},
		{name: "partial line", input: "unfinished", wantErr: io.EOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, err := ReadLine(t.Context(), strings.NewReader(tc.input))
			assert.Equal(t, tc.want, line)
			if tc.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.wantErr)
			}
		})
	}
}

type delayedReader struct {
	started chan struct{}
	release chan struct{}
	data    string
	err     error
}

func (r delayedReader) Read(p []byte) (int, error) {
	close(r.started)
	<-r.release
	return copy(p, r.data), r.err
}

func TestReadLineCancellationDoesNotStrandResult(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, data string
		err        error
	}{
		{name: "line", data: "hello\n"},
		{name: "error", err: io.EOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				r := delayedReader{started: make(chan struct{}), release: make(chan struct{}), data: tc.data, err: tc.err}
				defer close(r.release)
				returned := make(chan error, 1)
				go func() {
					_, err := ReadLine(ctx, r)
					returned <- err
				}()
				<-r.started
				cancel()
				require.ErrorIs(t, <-returned, context.Canceled)
				// The bubble must finish after the reader is released, including its result sender.
			})
		})
	}
}
