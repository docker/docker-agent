package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dgageot/rubocop-go/coptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func versionTestTree(t *testing.T) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "pkg", "config")
	for _, name := range []string{"v0", "v1", "v2", "latest", "legacy", "types"} {
		require.NoError(t, os.MkdirAll(filepath.Join(base, name), 0o755))
	}
	return base
}

func TestConfigVersionsRegistered(t *testing.T) {
	t.Parallel()
	base := versionTestTree(t)
	path := filepath.Join(base, "legacy", "register.go")
	src := `package legacy
func Register() { v0.Register(); v1.Register(); v2.Register() }
`
	assert.Empty(t, coptest.RunNamed(t, ConfigVersionsRegistered, path, src))
	offenses := coptest.RunNamed(t, ConfigVersionsRegistered, path, `package legacy
func Register() { v0.Register(); v1.Register() }
`)
	require.Len(t, offenses, 1)
	assert.Contains(t, offenses[0].Message, "v2")
	assert.NotContains(t, offenses[0].Message, "latest")

	latestPath := filepath.Join(base, "versions.go")
	assert.Empty(t, coptest.RunNamed(t, ConfigVersionsRegistered, latestPath, `package config
func versions() { latest.Register() }
`))
	offenses = coptest.RunNamed(t, ConfigVersionsRegistered, latestPath, "package config\nfunc versions() {}")
	require.Len(t, offenses, 1)
	assert.Contains(t, offenses[0].Message, "latest")
}

func TestConfigVersionBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path, source string
		want         int
	}{
		{"pkg/config/versions.go", `package config; import "github.com/docker/docker-agent/pkg/config/v2"`, 1},
		{"pkg/config/versions.go", `package config; import "github.com/docker/docker-agent/pkg/config/legacy"`, 1},
		{"pkg/config/versions.go", `package config; import "github.com/docker/docker-agent/pkg/config/latest"`, 0},
		{"pkg/config/versions_test.go", `package config; import "github.com/docker/docker-agent/pkg/config/legacy"`, 0},
		{"pkg/config/legacy/extra.go", `package legacy; import "github.com/docker/docker-agent/pkg/config"`, 1},
	} {
		t.Run(tc.path+tc.source, func(t *testing.T) {
			t.Parallel()
			assert.Len(t, coptest.RunNamed(t, ConfigVersionBoundary, tc.path, tc.source), tc.want)
		})
	}
}

func TestConfigVersionBridge(t *testing.T) {
	t.Parallel()
	base := versionTestTree(t)
	path := filepath.Join(base, "legacy", "register.go")
	for _, tc := range []struct {
		source string
		want   int
	}{
		{`package legacy
import previous "github.com/docker/docker-agent/pkg/config/v2"
func upgradeToLatest(c any) { _ = c.(previous.Config) }`, 0},
		{`package legacy
import previous "github.com/docker/docker-agent/pkg/config/v1"
func upgradeToLatest(c any) { _ = c.(previous.Config) }`, 1},
		{`package legacy
import "github.com/docker/docker-agent/pkg/config/v2"
func upgradeToLatest(c any) { _ = c.(v2.Config) }`, 0},
		{`package legacy
import "github.com/docker/docker-agent/pkg/config/v2"
func Register() { v2.Register() }`, 1},
	} {
		assert.Len(t, coptest.RunNamed(t, ConfigVersionBoundary, path, tc.source), tc.want)
	}
}

func TestConfigVersionImportFrozenChainAndLatestBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path, source string
		want         int
	}{
		{"pkg/config/latest/parse.go", `package latest; import "github.com/docker/docker-agent/pkg/config/v2"`, 1},
		{"pkg/config/latest/parse.go", `package latest; import "github.com/docker/docker-agent/pkg/config/types"`, 0},
		{"pkg/config/v2/parse.go", `package v2; import "github.com/docker/docker-agent/pkg/config/v1"`, 0},
		{"pkg/config/v2/parse.go", `package v2; import "github.com/docker/docker-agent/pkg/config/v0"`, 1},
		{"pkg/config/v0/parse.go", `package v0; import "github.com/docker/docker-agent/pkg/config/v1"`, 1},
	} {
		assert.Len(t, coptest.RunNamed(t, ConfigVersionImport, tc.path, tc.source), tc.want)
	}
}
