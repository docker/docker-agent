package dmr

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/model/provider/dmr/dmrmodels"
)

const testQwenBlobURL = "https://production.cloudfront.docker.com/registry-v2/docker/registry/v2/blobs/sha256/b5/b505f0cf69207567fdc6acec5a6d36303673a7da8cddf030f041677c85681729/data?Expires=1&Signature=x"

func TestCorruptPartial(t *testing.T) {
	digest := "b505f0cf69207567fdc6acec5a6d36303673a7da8cddf030f041677c85681729"

	t.Run("non-416 failure is ignored", func(t *testing.T) {
		_, _, ok := corruptPartial("Error: MANIFEST_UNKNOWN - Model not found")
		assert.False(t, ok)
	})

	t.Run("416 but no matching incomplete file", func(t *testing.T) {
		t.Setenv("DOCKER_CONFIG", t.TempDir())
		detail := "writing blob: ... GET " + testQwenBlobURL + ": 416 Requested Range Not Satisfiable"
		_, _, ok := corruptPartial(detail)
		assert.False(t, ok)
	})

	t.Run("416 with a matching incomplete file is detected", func(t *testing.T) {
		cfg := t.TempDir()
		t.Setenv("DOCKER_CONFIG", cfg)
		blobDir := filepath.Join(cfg, "models", "blobs", "sha256")
		require.NoError(t, os.MkdirAll(blobDir, 0o755))
		partial := filepath.Join(blobDir, digest+".incomplete")
		require.NoError(t, os.WriteFile(partial, []byte("0123456789"), 0o644))

		detail := "writing blob: read first byte: ... GET " + testQwenBlobURL + ": 416 Requested Range Not Satisfiable"
		path, size, ok := corruptPartial(detail)
		require.True(t, ok)
		assert.Equal(t, partial, path)
		assert.Equal(t, int64(10), size)
	})
}

func TestBuildPullErrorMessage_CorruptPartial(t *testing.T) {
	t.Parallel()

	partial := "/home/u/.docker/models/blobs/sha256/b505f0cf.incomplete"
	msg := buildPullErrorMessage("ai/qwen3", "... 416 Requested Range Not Satisfiable", partial, errors.New("exit status 1"))

	assert.Contains(t, msg, "Remove the corrupted partial download")
	assert.Contains(t, msg, "rm "+partial)
	assert.Contains(t, msg, "docker model pull ai/qwen3")
	// The generic "docker model rm" hint is replaced by the targeted one.
	assert.NotContains(t, msg, "docker model rm ai/qwen3")
}

func TestHumanizeBytes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "512 B", humanizeBytes(512))
	assert.Equal(t, "1.0 KiB", humanizeBytes(1024))
	assert.Equal(t, "6.2 GiB", humanizeBytes(6677640642))
}

func TestCleanPullStderr(t *testing.T) {
	t.Parallel()

	t.Run("empty input", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, cleanPullStderr(""))
		assert.Empty(t, cleanPullStderr("   \n\n  \n"))
	})

	t.Run("keeps the 416 failure line", func(t *testing.T) {
		t.Parallel()
		raw := "Downloaded 90.68MB of 91.74MB\nFailed to pull model: Error: writing blob: ... 416 Requested Range Not Satisfiable\n"
		got := cleanPullStderr(raw)
		assert.Contains(t, got, "416 Requested Range Not Satisfiable")
	})

	t.Run("collapses carriage-return progress rewrites", func(t *testing.T) {
		t.Parallel()
		raw := "Downloaded 1MB\rDownloaded 50MB\rDownloaded 91MB"
		got := cleanPullStderr(raw)
		assert.Equal(t, "Downloaded 91MB", got)
		assert.NotContains(t, got, "Downloaded 1MB")
		assert.NotContains(t, got, "Downloaded 50MB")
	})

	t.Run("strips ANSI escape sequences", func(t *testing.T) {
		t.Parallel()
		raw := "\x1b[32mpulling\x1b[0m\n\x1b[1mError:\x1b[0m boom"
		got := cleanPullStderr(raw)
		assert.NotContains(t, got, "\x1b")
		assert.Contains(t, got, "Error: boom")
	})

	t.Run("keeps only the last few lines", func(t *testing.T) {
		t.Parallel()
		var lines []string
		for i := range 25 {
			lines = append(lines, fmt.Sprintf("line-%02d", i))
		}
		got := cleanPullStderr(strings.Join(lines, "\n"))
		assert.LessOrEqual(t, len(strings.Split(got, "\n")), maxPullStderrLines)
		// The last line survives, early lines are dropped.
		assert.Contains(t, got, "line-24")
		assert.NotContains(t, got, "line-00")
		assert.NotContains(t, got, "line-19")
	})
}

func TestPullFailedError(t *testing.T) {
	t.Parallel()

	t.Run("renders model, detail and remediation", func(t *testing.T) {
		t.Parallel()
		err := &PullFailedError{
			Model:  "ai/qwen3",
			Detail: "Error: writing blob: ... 416 Requested Range Not Satisfiable",
			Cause:  errors.New("exit status 1"),
		}
		msg := err.Error()

		assert.Contains(t, msg, "failed to pull model ai/qwen3")
		assert.Contains(t, msg, "416 Requested Range Not Satisfiable")
		assert.Contains(t, msg, "docker model rm ai/qwen3")
		assert.Contains(t, msg, "docker model pull ai/qwen3")
		assert.Contains(t, msg, "docker model ls")
		// The new message must not reintroduce the old opaque wrapper.
		assert.NotContains(t, msg, "failed to get models:")
	})

	t.Run("empty detail falls back to the cause and stays actionable", func(t *testing.T) {
		t.Parallel()
		err := &PullFailedError{
			Model: "ai/qwen3",
			Cause: errors.New("exit status 1"),
		}
		msg := err.Error()

		assert.Contains(t, msg, "failed to pull model ai/qwen3")
		assert.Contains(t, msg, "exit status 1")
		assert.Contains(t, msg, "docker model rm ai/qwen3")
		assert.NotEmpty(t, strings.TrimSpace(msg))
	})

	t.Run("empty detail and nil cause is still non-empty", func(t *testing.T) {
		t.Parallel()
		err := &PullFailedError{Model: "ai/qwen3"}
		msg := err.Error()
		assert.Contains(t, msg, "failed to pull model ai/qwen3")
		assert.Contains(t, msg, "docker model rm ai/qwen3")
	})

	t.Run("errors.As matches and Unwrap returns the cause", func(t *testing.T) {
		t.Parallel()
		cause := errors.New("exit status 1")
		var err error = &PullFailedError{Model: "ai/qwen3", Cause: cause}

		var pfe *PullFailedError
		require.ErrorAs(t, err, &pfe)
		assert.Equal(t, "ai/qwen3", pfe.Model)
		assert.Equal(t, cause, errors.Unwrap(err))
	})

	t.Run("summary is a concise one-liner", func(t *testing.T) {
		t.Parallel()
		err := &PullFailedError{Model: "ai/qwen3", Detail: "noisy\nmultiline\ndetail"}
		summary := err.ModelPullErrorSummary()
		assert.Equal(t, "failed to pull model ai/qwen3", summary)
		assert.NotContains(t, summary, "\n")
	})
}

func TestPullUsesSelectedDockerConnection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell Docker shim")
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	t.Setenv("DMR_ARGS_FILE", argsFile)
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$DMR_ARGS_FILE"
[ "$1" = "--context=desktop-test" ] || exit 2
[ "$3" = "inspect" ] && exit 1
exit 0
`
	writeMockCLI(t, filepath.Join(dir, "docker"), script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx := dmrmodels.ContextWithDockerConnection(t.Context(), []string{"--context=desktop-test"}, nil)
	require.NoError(t, PullTo(ctx, "ai/test", io.Discard, io.Discard))
	got, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.Equal(t, "--context=desktop-test model inspect ai/test\n--context=desktop-test model pull ai/test\n", string(got))
}

func TestPullSelectedConnectionDoesNotRecoverLocalPartial(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell Docker shim")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\n' '" + testQwenBlobURL + ": 416 Requested Range Not Satisfiable' >&2\nexit 1\n"
	writeMockCLI(t, filepath.Join(dir, "docker"), script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	localConfig := t.TempDir()
	t.Setenv("DOCKER_CONFIG", localConfig)
	blobDir := filepath.Join(localConfig, "models", "blobs", "sha256")
	require.NoError(t, os.MkdirAll(blobDir, 0o755))
	partial := filepath.Join(blobDir, "b505f0cf69207567fdc6acec5a6d36303673a7da8cddf030f041677c85681729.incomplete")
	require.NoError(t, os.WriteFile(partial, []byte("unrelated download"), 0o600))

	for _, args := range [][]string{
		{"--context=remote-desktop"},
		{"--context=desktop-linux", "--config=" + t.TempDir()},
	} {
		ctx := dmrmodels.ContextWithDockerConnection(t.Context(), args, nil)
		err := PullTo(ctx, "ai/test", io.Discard, io.Discard)
		var failure *PullFailedError
		require.ErrorAs(t, err, &failure)
		assert.Empty(t, failure.CorruptPartial)
		assert.NotContains(t, err.Error(), partial)
		data, err := os.ReadFile(partial)
		require.NoError(t, err)
		assert.Equal(t, "unrelated download", string(data))
	}
}

func TestAutomaticPullOutputCanBeRedirected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell Docker shim")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n[ \"$2\" = inspect ] && exit 1\nprintf 'download progress\\n'\nexit 0\n"
	writeMockCLI(t, filepath.Join(dir, "docker"), script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r, w, err := os.Pipe()
	require.NoError(t, err)
	require.NoError(t, w.Close())
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; _ = r.Close() })
	var diagnostics bytes.Buffer
	ctx := WithPullOutput(t.Context(), &diagnostics)
	require.NoError(t, pullDockerModelIfNeeded(ctx, "ai/test"))
	assert.Contains(t, diagnostics.String(), "Pulling model ai/test")
	assert.Contains(t, diagnostics.String(), "download progress")
	assert.Contains(t, diagnostics.String(), "pulled successfully")
}
