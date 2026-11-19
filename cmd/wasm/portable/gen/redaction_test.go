package main

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRedactionRulesMatchPinnedModule(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("rule exporter requires a POSIX shell")
	}
	cmd := exec.CommandContext(t.Context(), sh, "redaction/generate.sh", "--check")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s; regenerate with sh cmd/wasm/portable/gen/redaction/generate.sh", output)
}
