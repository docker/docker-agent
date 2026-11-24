// Command wasmsmall prepares build-local dependencies for the small WASM profile.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type modulePatch struct {
	path, version, dir string
	apply              func(string) error
}

var patches = []modulePatch{
	{"github.com/anthropics/anthropic-sdk-go", "v1.84.0", ".anthropic-sdk", makeUnionsLazy},
	{"github.com/pb33f/ordered-map/v2", "v2.3.2", ".ordered-map", omitOrderedMapYAML},
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: wasmsmall <output directory>")
		os.Exit(2)
	}
	if err := prepare(context.Background(), os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "wasmsmall:", err)
		os.Exit(1)
	}
}

func prepare(ctx context.Context, out string) error {
	if err := os.MkdirAll(out, 0o750); err != nil {
		return err
	}
	args := []string{"mod", "edit"}
	for _, patch := range patches {
		dest, err := patch.copy(ctx, out)
		if err != nil {
			return err
		}
		args = append(args, "-replace="+patch.path+"@"+patch.version+"="+dest)
	}
	for _, name := range []string{"mod", "sum"} {
		data, err := os.ReadFile("go." + name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, "small."+name), data, 0o600); err != nil {
			return err
		}
	}
	_, err := goCommand(ctx, append(args, filepath.Join(out, "small.mod"))...)
	return err
}

func (p modulePatch) copy(ctx context.Context, out string) (string, error) {
	data, err := goCommand(ctx, "mod", "download", "-json", p.path)
	if err != nil {
		return "", err
	}
	var module struct {
		Dir, Version string
	}
	if err := json.Unmarshal(data, &module); err != nil {
		return "", err
	}
	if module.Version != p.version || module.Dir == "" {
		return "", fmt.Errorf("patch expects %s %s, got %s; review the patch before updating", p.path, p.version, module.Version)
	}

	// A fresh copy avoids stale files after upgrades or interrupted runs.
	dest, err := filepath.Abs(filepath.Join(out, p.dir))
	if err != nil {
		return "", err
	}
	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	if err := os.CopyFS(dest, os.DirFS(module.Dir)); err != nil {
		return "", err
	}
	if err := p.apply(dest); err != nil {
		return "", fmt.Errorf("patch %s: %w", p.path, err)
	}
	return dest, nil
}

func omitOrderedMapYAML(dir string) error {
	// Anthropic uses the ordered map for JSON schemas, not YAML.
	for _, name := range []string{"yaml.go", "yaml_test.go", "yaml_fuzz_test.go"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

func goCommand(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "go", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("go %v: %w\n%s", args, err, out)
	}
	return out, nil
}
