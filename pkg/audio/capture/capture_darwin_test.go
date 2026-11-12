//go:build darwin && cgo && !no_audio

package capture

import (
	"context"
	"os"
	"path/filepath"
	"runtime/cgo"
	"sync"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunCanceledBeforeStart(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	path := filepath.Join(t.TempDir(), "unused.wav")
	capturer := NewCapturer(SampleRate24000)
	for range 2 {
		err := capturer.Run(ctx, path, func([]byte) {
			t.Error("unexpected audio callback")
		}, func() {
			t.Error("unexpected readiness callback")
		})
		require.NoError(t, err)
	}
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRunInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sampleRate int
		path       string
		err        string
	}{
		{name: "zero rate", err: "invalid audio sample rate"},
		{name: "negative rate", sampleRate: -1, err: "invalid audio sample rate"},
		{name: "empty buffer", sampleRate: 9, err: "invalid audio sample rate"},
		{name: "oversized buffer", sampleRate: 1 << 40, err: "invalid audio sample rate"},
		{name: "NUL path", sampleRate: SampleRate24000, path: "audio\x00.wav", err: "NUL byte"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := NewCapturer(tt.sampleRate).Run(t.Context(), tt.path, nil, func() {
				t.Error("unexpected readiness callback")
			})
			require.ErrorContains(t, err, tt.err)
		})
	}
}

func TestRunFileCreationFailure(t *testing.T) {
	t.Parallel()

	capturer := NewCapturer(SampleRate24000)
	// File creation precedes queue creation, so this cannot access the microphone.
	path := filepath.Join(t.TempDir(), "missing", "audio.wav")
	for range 4 {
		t.Run("run", func(t *testing.T) {
			t.Parallel()
			err := capturer.Run(t.Context(), path, func([]byte) {
				t.Error("unexpected audio callback")
			}, func() {
				t.Error("unexpected readiness callback")
			})
			require.ErrorContains(t, err, "start audio capture")
		})
	}
}

func TestAudioCallbackCopiesData(t *testing.T) {
	t.Parallel()

	var received []byte
	handle := cgo.NewHandle(&callbacks{handler: func(data []byte) { received = data }})
	defer handle.Delete()

	data := []byte{1, 2, 3, 4}
	goAudioCallback(uintptr(handle), unsafe.Pointer(&data[0]), int32(len(data)), 0)
	data[0] = 9
	assert.Equal(t, []byte{1, 2, 3, 4}, received)
}

func TestAudioCallbacksAreIndependent(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })

	first := cgo.NewHandle(&callbacks{handler: func([]byte) {
		close(entered)
		<-release
	}})
	defer first.Delete()
	go func() {
		defer close(done)
		goAudioCallback(uintptr(first), nil, 0, 0)
	}()
	<-entered

	called := false
	second := cgo.NewHandle(&callbacks{handler: func([]byte) { called = true }})
	goAudioCallback(uintptr(second), nil, 0, 0)
	second.Delete()
	assert.True(t, called)
	select {
	case <-done:
		t.Error("first callback returned before its handler finished")
	default:
	}
	unblock.Do(func() { close(release) })
	<-done
}

func TestAudioCallbackReportsError(t *testing.T) {
	t.Parallel()

	cb := &callbacks{
		handler: func([]byte) { t.Error("unexpected audio callback") },
		errors:  make(chan int, 1),
	}
	handle := cgo.NewHandle(cb)
	defer handle.Delete()

	goAudioCallback(uintptr(handle), nil, 0, -1)
	goAudioCallback(uintptr(handle), nil, 0, -2)
	require.Equal(t, -1, <-cb.errors)
}

func TestAudioCallbackWithoutHandler(t *testing.T) {
	t.Parallel()

	handle := cgo.NewHandle(&callbacks{})
	defer handle.Delete()
	goAudioCallback(uintptr(handle), nil, 0, 0)
}
