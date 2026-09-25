package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
)

func TestLaunchName(t *testing.T) {
	t.Parallel()
	kitDir := t.TempDir()
	file := filepath.Join(kitDir, "workload.yaml")
	require.NoError(t, os.WriteFile(file, []byte("first"), 0o600))
	opts := Options{Template: "image:v1", Kits: []string{kitDir}, KitArgs: []string{"x=a,b"}}
	name, err := launchName("/work", []string{"/a", "/b"}, "", opts)
	require.NoError(t, err)
	assert.Regexp(t, `^docker-agent-[a-f0-9]{24}$`, name)
	reordered, err := launchName("/work", []string{"/b", "/a"}, "", opts)
	require.NoError(t, err)
	assert.Equal(t, name, reordered)

	for _, change := range []Options{
		{Template: "image:v2", Kits: opts.Kits, KitArgs: opts.KitArgs},
		{Template: opts.Template, Kits: opts.Kits, KitArgs: []string{"x=b,a"}},
		{Workload: "docker/workload:v1", Kits: opts.Kits, KitArgs: opts.KitArgs},
	} {
		got, err := launchName("/work", []string{"/a", "/b"}, "", change)
		require.NoError(t, err)
		assert.NotEqual(t, name, got)
	}
	require.NoError(t, os.WriteFile(file, []byte("second"), 0o600))
	changed, err := launchName("/work", []string{"/a", "/b"}, "", opts)
	require.NoError(t, err)
	assert.NotEqual(t, name, changed, "local kit edits must not silently reuse an old kit")
}

func TestSameWorkspaces(t *testing.T) {
	t.Parallel()
	wanted := []string{"/work", "/config:ro", "/kit:ro"}
	assert.True(t, sameWorkspaces([]string{"/work", "/kit:ro", "/config:ro"}, wanted))
	assert.False(t, sameWorkspaces([]string{"/work", "/kit:ro", "/config"}, wanted))
	assert.False(t, sameWorkspaces([]string{"/work", "/kit:ro", "/config:ro", "/private"}, wanted))
	assert.False(t, sameWorkspaces(nil, wanted))
}

func TestConcurrentLoginKitIdentity(t *testing.T) {
	paths.SetCacheDir(t.TempDir())
	t.Cleanup(func() { paths.SetCacheDir("") })
	const count = 32
	results := make(chan string, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for range count {
		wg.Go(func() {
			dir, err := LoginKit("https://api.docker.com", true)
			if err != nil {
				errs <- err
				return
			}
			name, err := launchName("/work", []string{dir}, dir, Options{Workload: "docker/workload:v3"})
			if err != nil {
				errs <- err
				return
			}
			results <- name
		})
	}
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Len(t, results, count)
	want := <-results
	for name := range results {
		assert.Equal(t, want, name)
	}
}

func TestLaunchNameFileBoundaries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	require.NoError(t, os.WriteFile(a, []byte("alpha"), 0o600))
	require.NoError(t, os.WriteFile(b, []byte("beta"), 0o600))
	opts := Options{Kits: []string{dir}}
	before, err := launchName("/work", nil, "", opts)
	require.NoError(t, err)
	info, err := os.Stat(b)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(a, fmt.Appendf(nil, "alpha%s\x00%s\x00beta", b, info.Mode()), 0o600))
	require.NoError(t, os.Remove(b))
	after, err := launchName("/work", nil, "", opts)
	require.NoError(t, err)
	assert.NotEqual(t, before, after, "file content must not impersonate another file's framing")
}

func TestLaunchNameIgnoresGitMetadata(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kit.yaml"), []byte("kit"), 0o600))
	opts := Options{Kits: []string{dir}}
	before, err := launchName("/work", nil, "", opts)
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main"), 0o600))
	after, err := launchName("/work", nil, "", opts)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestKitFlagsCSVContract(t *testing.T) {
	t.Parallel()
	opts := Options{Kits: []string{"./mix,in", `./kit"quoted`}, KitArgs: []string{"list=a,b"}}
	flags := pflag.NewFlagSet("sbx", pflag.ContinueOnError)
	kits := flags.StringSlice("kit", nil, "")
	args := flags.StringArray("kit-arg", nil, "")
	require.NoError(t, flags.Parse(opts.kitFlags()))
	assert.Equal(t, opts.Kits, *kits)
	assert.Equal(t, opts.KitArgs, *args)
}
