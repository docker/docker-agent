package modelsdev

import (
	_ "embed"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"
)

//go:generate go run ./internal/gen

// snapshotJSON is a snapshot of the models.dev catalog, refreshed at build
// time by `go generate ./pkg/modelsdev/...` (see internal/gen). It is the
// last-resort fallback used when neither the in-memory catalog, the on-disk
// cache, nor a live fetch is available — so a fresh binary always has a
// reasonable model catalog instead of a hard-coded default.
//
//go:embed snapshot.json
var snapshotJSON []byte

// snapshotDate records when snapshotJSON was generated (RFC3339, UTC). It is
// used to surface staleness and by tests that guard the snapshot's freshness.
//
//go:embed snapshot_date.txt
var snapshotDateRaw string

var (
	snapshotOnce sync.Once
	snapshotDB   *Database
)

// embeddedSnapshot returns the catalog baked into the binary at build time.
// The JSON is parsed once and cached. A malformed snapshot yields an empty
// (non-nil) database rather than a panic, so a bad embed degrades to
// "provider not found" instead of crashing the process. The parse failure is
// logged because, with the hardcoded context-limit fallback gone, a corrupt
// snapshot would otherwise silently clamp large-context models with no signal.
func embeddedSnapshot() *Database {
	snapshotOnce.Do(func() {
		var providers map[string]Provider
		if err := json.Unmarshal(snapshotJSON, &providers); err != nil {
			slog.Warn("embedded models.dev snapshot is malformed; context-window limits will be wrong", "error", err)
			snapshotDB = &Database{}
			return
		}
		snapshotDB = &Database{Providers: providers}
	})
	return snapshotDB
}

// EmbeddedSnapshot returns the models.dev catalog committed to the repo at
// pkg/modelsdev/snapshot.json and baked into the binary at build time. It
// never touches the network or the filesystem cache, which makes it the
// right catalog for hermetic tests that must validate against exactly what
// is checked in — e.g. TestParseExamples — rather than whatever the live
// models.dev API happens to say at the instant the test runs.
//
// The returned *Database is a shared, process-wide singleton: callers must
// treat it as read-only.
func EmbeddedSnapshot() *Database {
	return embeddedSnapshot()
}

// SnapshotDate returns the time the embedded models.dev snapshot was
// generated. The zero value is returned when the recorded date can't be
// parsed.
func SnapshotDate() time.Time {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(snapshotDateRaw))
	if err != nil {
		return time.Time{}
	}
	return t
}
