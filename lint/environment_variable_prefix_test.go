package main

import (
	"testing"

	"github.com/dgageot/rubocop-go/coptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvironmentVariablePrefixFlagsUnpairedLegacyName(t *testing.T) {
	t.Parallel()
	src := `package p
import "os"
func f() string { return os.Getenv("CAGENT_PPROF_ADDR") }
`
	offenses := coptest.Run(t, EnvironmentVariablePrefix, src)
	require.Len(t, offenses, 1)
	assert.Equal(t, "Lint/EnvironmentVariablePrefix", offenses[0].CopName)
	assert.Contains(t, offenses[0].Message, "DOCKER_AGENT_PPROF_ADDR")
}

func TestEnvironmentVariablePrefixAllowsLegacyAlias(t *testing.T) {
	t.Parallel()
	src := `package p
const (
	current = "DOCKER_AGENT_CONFIG_DIR"
	legacy = "CAGENT_CONFIG_DIR"
)
`
	assert.Empty(t, coptest.Run(t, EnvironmentVariablePrefix, src))
}

func TestEnvironmentVariablePrefixRequiresMatchingAlias(t *testing.T) {
	t.Parallel()
	src := `package p
const (
	current = "DOCKER_AGENT_OTHER"
	legacy = "CAGENT_CONFIG_DIR"
)
`
	require.Len(t, coptest.Run(t, EnvironmentVariablePrefix, src), 1)
}

func TestEnvironmentVariablePrefixAllowsInternalAskpassVariables(t *testing.T) {
	t.Parallel()
	src := `package p
const (
	socket = "CAGENT_ASKPASS_SOCKET"
	token = "CAGENT_ASKPASS_TOKEN"
)
`
	assert.Empty(t, coptest.Run(t, EnvironmentVariablePrefix, src))
}

func TestEnvironmentVariablePrefixIgnoresNonEnvironmentStrings(t *testing.T) {
	t.Parallel()
	src := `package p
const a = "CAGENT_"
const b = "CAGENT_mixed_case"
const c = "prefix CAGENT_FOO"
`
	assert.Empty(t, coptest.Run(t, EnvironmentVariablePrefix, src))
}
