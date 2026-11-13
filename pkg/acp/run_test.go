package acp

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
)

func TestRunMalformedInput(t *testing.T) {
	t.Parallel()

	input := strings.NewReader(strings.Repeat("not JSON\n", 100))
	err := Run(t.Context(), "minimal", input, io.Discard, &config.RuntimeConfig{}, ":memory:")
	require.NoError(t, err)
}
