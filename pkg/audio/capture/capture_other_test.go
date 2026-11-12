//go:build !darwin || !cgo || no_audio

package capture

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunNotSupported(t *testing.T) {
	t.Parallel()

	capturer := NewCapturer(SampleRate24000)
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		if canceled {
			cancel()
		}
		err := capturer.Run(ctx, "unused.wav", func([]byte) {
			t.Error("unexpected audio callback")
		}, func() {
			t.Error("unexpected readiness callback")
		})
		cancel()
		require.ErrorIs(t, err, ErrNotSupported)
	}
}
