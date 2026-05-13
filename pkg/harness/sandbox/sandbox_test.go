package sandbox_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker-agent/pkg/harness/sandbox"
)

func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path // file may not exist yet
	}
	return resolved
}

func TestResolveSimple(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := sandbox.Resolve(root, "file.txt")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := realPath(t, path)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveAbsoluteInside(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sub", "file.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := sandbox.Resolve(root, path)
	if err != nil {
		t.Fatalf("Resolve absolute inside: %v", err)
	}
	want := realPath(t, path)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveDotDotEscape(t *testing.T) {
	root := t.TempDir()
	_, err := sandbox.Resolve(root, "../etc/passwd")
	if !errors.Is(err, sandbox.ErrEscape) {
		t.Errorf("expected ErrEscape, got %v", err)
	}
}

func TestResolveAbsoluteOutside(t *testing.T) {
	root := t.TempDir()
	_, err := sandbox.Resolve(root, "/etc/passwd")
	if !errors.Is(err, sandbox.ErrEscape) {
		t.Errorf("expected ErrEscape for /etc/passwd, got %v", err)
	}
}

func TestResolveSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	// Create a symlink inside root that points outside.
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	_, err := sandbox.Resolve(root, "escape")
	if !errors.Is(err, sandbox.ErrEscape) {
		t.Errorf("expected ErrEscape for symlink escape, got %v", err)
	}
}

func TestResolveNonExistentFileInRoot(t *testing.T) {
	root := t.TempDir()
	// File doesn't exist yet -- should succeed (write target).
	got, err := sandbox.Resolve(root, "newfile.txt")
	if err != nil {
		t.Fatalf("Resolve non-existent: %v", err)
	}
	realRoot := realPath(t, root)
	expected := filepath.Join(realRoot, "newfile.txt")
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestResolveRoot(t *testing.T) {
	root := t.TempDir()
	got, err := sandbox.Resolve(root, ".")
	if err != nil {
		t.Fatalf("Resolve root: %v", err)
	}
	want := realPath(t, root)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAllowedEnvFiltersSecrets(t *testing.T) {
	env := map[string]string{
		"HOME":                  "/home/user",
		"PATH":                  "/usr/bin",
		"AWS_SECRET_ACCESS_KEY": "secret",
		"DATABASE_URL":          "postgres://...",
		"ANTHROPIC_API_KEY":     "sk-ant-...",
	}

	filtered := sandbox.AllowedEnv(env, nil)

	if _, ok := filtered["AWS_SECRET_ACCESS_KEY"]; ok {
		t.Error("AWS_SECRET_ACCESS_KEY should be filtered")
	}
	if _, ok := filtered["DATABASE_URL"]; ok {
		t.Error("DATABASE_URL should be filtered")
	}
	if filtered["HOME"] != "/home/user" {
		t.Error("HOME should be preserved")
	}
	if filtered["ANTHROPIC_API_KEY"] != "sk-ant-..." {
		t.Error("ANTHROPIC_API_KEY should be preserved (not in sensitive list)")
	}
}

func TestAllowedEnvExplicitAllow(t *testing.T) {
	env := map[string]string{
		"AWS_SECRET_ACCESS_KEY": "secret",
	}

	filtered := sandbox.AllowedEnv(env, []string{"AWS_SECRET_ACCESS_KEY"})

	if filtered["AWS_SECRET_ACCESS_KEY"] != "secret" {
		t.Error("explicitly allowed key should be preserved")
	}
}
