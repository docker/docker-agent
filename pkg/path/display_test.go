package path

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRelativeTo(t *testing.T) {
	t.Parallel()
	base := t.TempDir()

	assert.Equal(t, filepath.Join("dir", "file.txt"), RelativeTo(filepath.Join(base, "dir", "file.txt"), base))
	assert.Equal(t, filepath.Join("..", "sibling"), RelativeTo(filepath.Join(filepath.Dir(base), "sibling"), base))
	assert.Equal(t, filepath.Clean("relative/path"), RelativeTo("relative/path", base))
	assert.Equal(t, filepath.Join(base, "file.txt"), RelativeTo(filepath.Join(base, "file.txt"), "relative/base"))
}

func TestShortenHomeUsesPlatformNativeHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	assert.Equal(t, filepath.Join("~", "file.txt"), ShortenHome(filepath.Join(home, "file.txt")))

	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	assert.Equal(t, "a/../b", ShortenHome("a/../b"))
}

func TestShortenHomeDir(t *testing.T) {
	t.Parallel()
	home := t.TempDir()

	assert.Equal(t, "~", ShortenHomeDir(home, home))
	assert.Equal(t, filepath.Join("~", ".agents", "skills", "commit"), ShortenHomeDir(filepath.Join(home, ".agents", "skills", "commit"), home))
	assert.Equal(t, filepath.Join(filepath.Dir(home), "elsewhere"), ShortenHomeDir(filepath.Join(filepath.Dir(home), "elsewhere"), home))
}

func TestIsWithin(t *testing.T) {
	t.Parallel()
	base := t.TempDir()

	assert.True(t, IsWithin(base, base))
	assert.True(t, IsWithin(filepath.Join(base, "child"), base))
	assert.False(t, IsWithin(filepath.Dir(base), base))
	assert.False(t, IsWithin(filepath.Join(filepath.Dir(base), filepath.Base(base)+"-suffix"), base))
}
