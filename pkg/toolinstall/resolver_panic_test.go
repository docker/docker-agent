package toolinstall

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withPanickingInstaller swaps doInstallFn for a panicking stub for the
// duration of the test, restoring the original on cleanup.
func withPanickingInstaller(t *testing.T, panicValue any) {
	t.Helper()
	original := doInstallFn
	doInstallFn = func(context.Context, string, string) (string, error) {
		panic(panicValue)
	}
	t.Cleanup(func() { doInstallFn = original })
}

// TestSafeInstall_RecoversFromStringPanic verifies that a string panic
// inside doInstall is converted to an error rather than crashing the
// process via singleflight's re-panic.
func TestSafeInstall_RecoversFromStringPanic(t *testing.T) {
	withPanickingInstaller(t, "boom")

	path, err := safeInstall(t.Context(), "fake-tool", "")

	require.Error(t, err)
	assert.Empty(t, path)
	assert.Contains(t, err.Error(), "fake-tool")
	assert.Contains(t, err.Error(), "panicked")
	assert.Contains(t, err.Error(), "boom")
}

// TestSafeInstall_RecoversFromNilDeref verifies recovery from a runtime
// nil-pointer dereference, which is the failure mode described in the
// original issue (downstream HTTP/YAML code dereferencing a nil result).
func TestSafeInstall_RecoversFromNilDeref(t *testing.T) {
	original := doInstallFn
	doInstallFn = func(context.Context, string, string) (string, error) {
		var p *struct{ X int }
		_ = p.X // forces a runtime panic
		return "", nil
	}
	t.Cleanup(func() { doInstallFn = original })

	path, err := safeInstall(t.Context(), "fake-tool", "")

	require.Error(t, err)
	assert.Empty(t, path)
	assert.Contains(t, err.Error(), "panicked")
}

// TestResolve_ConcurrentPanic_DoesNotCrash exercises the full resolve()
// path: multiple goroutines hit singleflight at once, the underlying
// install panics, and every caller must observe an error rather than the
// process crashing via singleflight's `go panic(...)` re-raise.
func TestResolve_ConcurrentPanic_DoesNotCrash(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())
	withPanickingInstaller(t, "simulated network failure")

	const numGoroutines = 10
	var wg sync.WaitGroup
	errs := make([]error, numGoroutines)

	for i := range numGoroutines {
		wg.Go(func() {
			_, errs[i] = resolve(t.Context(), "concurrent-panic-tool", "")
		})
	}
	wg.Wait()

	for i, err := range errs {
		require.Errorf(t, err, "goroutine %d should observe an error, not crash", i)
		assert.Contains(t, err.Error(), "panicked")
	}
}
