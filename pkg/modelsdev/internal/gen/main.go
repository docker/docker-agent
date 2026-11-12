// Command gen refreshes the models.dev catalog snapshot embedded in the
// docker-agent binary.
//
// It fetches https://models.dev/api.json, re-marshals it through the trimmed
// [modelsdev.Database] type (dropping fields docker-agent never reads) and
// writes the result to pkg/modelsdev/embedded/snapshot.json together with a generated
// snapshot_date.txt recording when the snapshot was taken.
//
// Run it via `go generate ./pkg/modelsdev/...` or the `task update-models`
// target. The fetch is best-effort: when models.dev is unreachable the
// existing snapshot is left untouched so offline builds keep working.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/docker-agent/pkg/modelsdev"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: keeping existing models.dev snapshot: %v\n", err)
		// Best-effort: never fail the build just because models.dev is down.
		return
	}
}

func run() error {
	outDir, err := snapshotDir()
	if err != nil {
		return err
	}

	// Standalone snapshot generator (its own main); the build-time root
	// context has no parent to derive from.
	//rubocop:disable Lint/ContextConnectivity
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	providers, err := fetch(ctx)
	if err != nil {
		return err
	}
	if len(providers) == 0 {
		return errors.New("models.dev returned an empty catalog")
	}

	// Re-marshal through the trimmed Database type so the snapshot only
	// carries the fields docker-agent actually reads. Compact (un-indented)
	// JSON keeps the embedded blob — and the binary — as small as possible.
	data, err := json.Marshal(modelsdev.Database{Providers: providers}.Providers)
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}

	snapshotPath := filepath.Join(outDir, "snapshot.json")
	if err := os.WriteFile(snapshotPath, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", snapshotPath, err)
	}

	datePath := filepath.Join(outDir, "snapshot_date.txt")
	date := time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(datePath, []byte(date+"\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", datePath, err)
	}

	fmt.Printf("Wrote %d providers to %s (%s)\n", len(providers), snapshotPath, date)
	return nil
}

func fetch(ctx context.Context) (map[string]modelsdev.Provider, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsdev.ModelsDevAPIURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", modelsdev.ModelsDevAPIURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var providers map[string]modelsdev.Provider
	if err := json.Unmarshal(body, &providers); err != nil {
		return nil, fmt.Errorf("decode catalog: %w", err)
	}
	return providers, nil
}

// snapshotDir locates pkg/modelsdev/embedded relative to this source file so the
// generator works regardless of the caller's working directory.
func snapshotDir() (string, error) {
	// This file lives at pkg/modelsdev/internal/gen/main.go; the snapshot
	// lives under the sibling embedded directory.
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	// `go run ./pkg/modelsdev/internal/gen` runs with the module root as wd.
	candidate := filepath.Join(wd, "pkg", "modelsdev", "embedded")
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}
	// `go generate` runs with pkg/modelsdev/embedded as wd.
	if _, err := os.Stat(filepath.Join(wd, "snapshot.json")); err == nil {
		return wd, nil
	}
	return "", fmt.Errorf("could not locate pkg/modelsdev/embedded from %s", wd)
}
