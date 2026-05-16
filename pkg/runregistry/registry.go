// Package runregistry persists discovery records for running docker-agent
// processes that expose a control plane (see run --listen). Records live as
// per-pid JSON files under <data dir>/runs so external tools can enumerate
// live runs and connect to them.
package runregistry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker-agent/pkg/paths"
)

// Record describes a running docker-agent that exposes a control plane.
type Record struct {
	PID       int       `json:"pid"`
	Addr      string    `json:"addr"`
	SessionID string    `json:"session_id"`
	Agent     string    `json:"agent,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// Dir is the directory holding run records.
func Dir() string {
	return filepath.Join(paths.GetDataDir(), "runs")
}

// Write atomically persists a record for the current process and returns a
// cleanup func that removes it. Cleanup is safe to call more than once.
//
// The registry directory is created with 0o700 so other local users cannot
// enumerate live PIDs/addresses by listing it. Individual records are still
// written with 0o600 for the same reason. Writes go through a sibling temp
// file + rename so concurrent readers never see torn JSON.
func Write(rec Record) (func(), error) {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return nil, fmt.Errorf("creating run registry dir: %w", err)
	}

	path := filepath.Join(Dir(), strconv.Itoa(rec.PID)+".json")
	buf, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(path, buf, 0o600); err != nil {
		return nil, err
	}

	return func() { _ = os.Remove(path) }, nil
}

// writeAtomic writes data to path through a sibling temp file + rename so
// readers never observe a partially-written file.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir, name := filepath.Split(path)
	tmp, err := os.CreateTemp(dir, name+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// List returns every record currently registered. Stale records (whose pid is
// no longer alive) are skipped and best-effort removed.
func List() ([]Record, error) {
	entries, err := os.ReadDir(Dir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var out []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(Dir(), e.Name())
		buf, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec Record
		if err := json.Unmarshal(buf, &rec); err != nil {
			continue
		}
		if !pidAlive(rec.PID) {
			_ = os.Remove(path)
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// Latest returns the most recently started live record, or false when none.
func Latest() (Record, bool, error) {
	records, err := List()
	if err != nil || len(records) == 0 {
		return Record{}, false, err
	}
	latest := slices.MaxFunc(records, func(a, b Record) int {
		return a.StartedAt.Compare(b.StartedAt)
	})
	return latest, true, nil
}

// ErrNoRun is returned when no live run can be found that satisfies the
// caller's request (empty registry, or no record matches the target).
var ErrNoRun = errors.New("no live docker-agent run found; start one with: docker-agent run --listen 127.0.0.1:0")

// Find resolves a target reference to a single live record.
//
// An empty target returns the most recently started run. A numeric target is
// matched by PID; a target starting with "http://" or "https://" is matched
// against record addresses; anything else is matched as a (possibly partial)
// session ID. PID and address matches are exact. Session-ID matching prefers
// exact equality and only falls back to substring matching when no record
// matches exactly; ambiguous substring matches return an error so callers
// don't act on the wrong session.
func Find(target string) (Record, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		rec, ok, err := Latest()
		if err != nil {
			return Record{}, err
		}
		if !ok {
			return Record{}, ErrNoRun
		}
		return rec, nil
	}

	records, err := List()
	if err != nil {
		return Record{}, err
	}
	if len(records) == 0 {
		return Record{}, ErrNoRun
	}

	if pid, err := strconv.Atoi(target); err == nil {
		for _, r := range records {
			if r.PID == pid {
				return r, nil
			}
		}
		return Record{}, fmt.Errorf("no live run with pid %d: %w", pid, ErrNoRun)
	}

	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		want := strings.TrimRight(target, "/")
		for _, r := range records {
			if strings.TrimRight(r.Addr, "/") == want {
				return r, nil
			}
		}
		return Record{}, fmt.Errorf("no live run at %s: %w", target, ErrNoRun)
	}

	// Prefer an exact session-id match: an unambiguous full id must always
	// resolve, even when other ids contain it as a substring.
	for _, r := range records {
		if r.SessionID == target {
			return r, nil
		}
	}
	var matches []Record
	for _, r := range records {
		if strings.Contains(r.SessionID, target) {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return Record{}, fmt.Errorf("no live run matches %q (pid, http URL, or session id): %w", target, ErrNoRun)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, 0, len(matches))
		for _, r := range matches {
			ids = append(ids, r.SessionID)
		}
		return Record{}, fmt.Errorf("ambiguous target %q: matches sessions %s", target, strings.Join(ids, ", "))
	}
}

// pidAlive reports whether the given pid corresponds to a live process.
// Uses os.FindProcess + signal 0, the cross-platform idiom for liveness.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
