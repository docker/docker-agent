package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/dgageot/rubocop-go/config"
	"github.com/dgageot/rubocop-go/cop"
	"github.com/dgageot/rubocop-go/coptest"
	"github.com/dgageot/rubocop-go/runner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOTelTracerNameFlagsCagent(t *testing.T) {
	t.Parallel()
	src := `package p
import "go.opentelemetry.io/otel"
func f() { _ = otel.Tracer("cagent") }
`
	offenses := coptest.RunTyped(t, OTelTracerName, src)
	require.Len(t, offenses, 1)
	assert.Equal(t, "Lint/OTelTracerName", offenses[0].CopName)
	assert.Contains(t, offenses[0].Message, `otel.Tracer(AppName)`)
}

func TestOTelTracerNameFlagsLocalAppName(t *testing.T) {
	t.Parallel()
	// A local const named AppName from an unrelated package must not bypass the cop.
	src := `package p
import "go.opentelemetry.io/otel"
const AppName = "docker-agent"
func f() { _ = otel.Tracer(AppName) }
`
	offenses := coptest.RunTyped(t, OTelTracerName, src)
	require.Len(t, offenses, 1)
	assert.Contains(t, offenses[0].Message, `got "docker-agent"`)
}

func TestOTelTracerNameAllowsVersionAppName(t *testing.T) {
	t.Parallel()
	src := `package p
import (
	"go.opentelemetry.io/otel"
	"github.com/docker/docker-agent/pkg/version"
)
func f() { _ = otel.Tracer(version.AppName) }
`
	assert.Empty(t, coptest.RunTyped(t, OTelTracerName, src))
}

func TestOTelTracerNameFlagsWrongName(t *testing.T) {
	t.Parallel()
	src := `package p
import "go.opentelemetry.io/otel"
func f() { _ = otel.Tracer("wrong") }
`
	offenses := coptest.RunTyped(t, OTelTracerName, src)
	require.Len(t, offenses, 1)
	assert.Contains(t, offenses[0].Message, `got "wrong"`)
}

func TestOTelTracerNameRunnerAllowsRootAppName(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	const src = `package root
import "go.opentelemetry.io/otel"
const AppName = "docker-agent"
func f() { _ = otel.Tracer(AppName) }
`
	for name, content := range map[string]string{
		"go.mod":              "module github.com/docker/docker-agent\n\ngo 1.27\nrequire go.opentelemetry.io/otel v0.0.0\nreplace go.opentelemetry.io/otel => ./otel\n",
		"otel/go.mod":         "module go.opentelemetry.io/otel\n\ngo 1.27\n",
		"otel/tracer.go":      "package otel\nfunc Tracer(string) any { return nil }\n",
		"cmd/root/tracer.go":  src,
		"pkg/other/tracer.go": src,
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
		require.NoError(t, os.WriteFile(name, []byte(content), 0o600))
	}
	var output bytes.Buffer
	r := runner.New([]cop.Cop{OTelTracerName}, config.DefaultConfig(), &output)
	count, err := r.Run([]string{"."})
	require.NoError(t, err)
	assert.Equal(t, 1, count, output.String())
	assert.Contains(t, filepath.ToSlash(output.String()), "pkg/other/tracer.go")
	assert.NotContains(t, filepath.ToSlash(output.String()), "cmd/root/tracer.go")
}
