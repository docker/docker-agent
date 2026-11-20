package markdowntransform

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const helperTimeoutMS = 60_000

func pidOf(t *testing.T, out string) string {
	t.Helper()
	pid, _, ok := strings.Cut(out, ":")
	require.True(t, ok, out)
	return pid
}

func TestCommandTransformerReusesProcess(t *testing.T) {
	t.Parallel()
	transformer := newCommandTransformer(helperConfig(t, "echo", helperTimeoutMS))
	t.Cleanup(transformer.restart)
	first, err := transformer.transform(t.Context(), Input{Markdown: "one"})
	require.NoError(t, err)
	second, err := transformer.transform(t.Context(), Input{Markdown: "two"})
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(first, ":one"))
	assert.True(t, strings.HasSuffix(second, ":two"))
	assert.Equal(t, pidOf(t, first), pidOf(t, second))
}

func TestCommandTransformerKeepsProcessAfterErrorReply(t *testing.T) {
	t.Parallel()
	transformer := newCommandTransformer(helperConfig(t, "error-once", helperTimeoutMS))
	t.Cleanup(transformer.restart)
	_, err := transformer.transform(t.Context(), Input{Markdown: "one"})
	var reply *rpcError
	require.ErrorAs(t, err, &reply)
	require.NotNil(t, transformer.client, "a well-formed error reply must not restart the process")
	out, err := transformer.transform(t.Context(), Input{Markdown: "two"})
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(out, ":two"))
}

func TestCommandTransformerRestartsAfterFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ mode, name string }{{"hang", "timeout"}, {"exit", "exit"}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			timeout := helperTimeoutMS
			if tc.mode == "hang" {
				timeout = 300
			}
			transformer := newCommandTransformer(helperConfig(t, tc.mode, timeout))
			t.Cleanup(transformer.restart)
			_, err := transformer.transform(t.Context(), Input{Markdown: "one"})
			require.Error(t, err)
			assert.Nil(t, transformer.client)
			_, err = transformer.transform(t.Context(), Input{Markdown: "two"})
			require.ErrorContains(t, err, "restarting", "restarts are rate limited")
			assert.WithinDuration(t, time.Now().Add(restartDelay), transformer.retryAt, restartDelay)
		})
	}
}

func TestCommandTransformerStartFailure(t *testing.T) {
	t.Parallel()
	transformer := newCommandTransformer(Config{Command: "exit 0", WorkingDir: t.TempDir() + "/missing"})
	_, err := transformer.transform(t.Context(), Input{Markdown: "one"})
	require.Error(t, err)
	assert.Nil(t, transformer.client)
}

func TestStart(t *testing.T) {
	t.Parallel()
	assert.Nil(t, Start(t.Context(), nil))
	assert.Nil(t, Start(t.Context(), []Config{{Command: " "}}))
	display := Start(t.Context(), []Config{helperConfig(t, "echo", helperTimeoutMS), helperConfig(t, "echo", helperTimeoutMS)})
	require.NotNil(t, display)
	var surface Surface
	changed := display.Changed()
	assert.Equal(t, "source", display.Markdown(&surface, Input{Markdown: "source"}))
	<-changed
	out := display.Markdown(&surface, Input{Markdown: "source"})
	assert.Equal(t, 2, strings.Count(out, ":"), "transformers run in order: %q", out)
	assert.True(t, strings.HasSuffix(out, ":source"))
}
