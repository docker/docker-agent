package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dgageot/rubocop-go/config"
	"github.com/dgageot/rubocop-go/prog"
	"github.com/dgageot/rubocop-go/runner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCorrectnessCopSuppressions(t *testing.T, check prog.Cop, source string) {
	t.Helper()
	dir := t.TempDir()
	// The runner resolves relative module patterns from its current directory.
	t.Chdir(dir)
	for name, data := range map[string]string{
		"go.mod":               "module github.com/docker/docker-agent\n\ngo 1.27\n",
		"reported/sample.go":   source,
		"inline/sample.go":     strings.ReplaceAll(source, "// offense", "//rubocop:disable "+check.Name()),
		"suppressed/sample.go": "//rubocop:disable-file " + check.Name() + "\n" + source,
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
		require.NoError(t, os.WriteFile(name, []byte(data), 0o600))
	}
	var output bytes.Buffer
	r := runner.New(nil, config.DefaultConfig(), &output).WithProgramCops([]prog.Cop{check})
	count, err := r.Run([]string{"."})
	require.NoError(t, err)
	assert.Equal(t, 1, count, output.String())
	assert.Contains(t, output.String(), filepath.Join("reported", "sample.go"))
	assert.NotContains(t, output.String(), filepath.Join("inline", "sample.go"))
	assert.NotContains(t, output.String(), filepath.Join("suppressed", "sample.go"))
}
