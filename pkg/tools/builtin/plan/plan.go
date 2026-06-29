// Package plan exposes a per-session plan tracker for the "draft, approve,
// execute" workflow. One markdown plan per session, stored at
// <data-dir>/plans/<session-id>.md. The toolset is a metadata stub — the
// runtime owns the handlers (see pkg/runtime/plan_handlers.go) so they can
// reach the live session, the elicitation bridge and the agent router.
package plan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/atomicfile"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameWritePlan    = "write_plan"
	ToolNameReadPlan     = "read_plan"
	ToolNameExitPlanMode = "exit_plan_mode"
)

// Names retained from the pre-session-mode toolset so the existing TUI
// renderer registry compiles unchanged. The runtime no longer advertises or
// handles these, so the model never calls them; they exist only as string
// constants for callers that still import them. Drop when the TUI's plan
// renderer is updated.
const (
	ToolNameListPlans          = "list_plans"
	ToolNameDeletePlan         = "delete_plan"
	ToolNameSetPlanStatus      = "set_plan_status"
	ToolNameGetPlanStatus      = "get_plan_status"
	ToolNameUpdatePlanFromFile = "update_plan_from_file"
	ToolNameExportPlanToFile   = "export_plan_to_file"
)

// sessionIDPattern rejects anything that could escape the plans directory
// ('/', '\\', '..'). 128 chars is well above any realistic ID.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

const maxPlanAge = 30 * 24 * time.Hour

var (
	ErrInvalidSessionID = errors.New("invalid session ID")
	ErrPlanNotFound     = errors.New("plan not found")
)

func DefaultDir() string {
	return filepath.Join(paths.GetDataDir(), "plans")
}

type ToolSet struct{}

var (
	_ tools.ToolSet      = (*ToolSet)(nil)
	_ tools.Instructable = (*ToolSet)(nil)
)

func CreateToolSet() (tools.ToolSet, error) {
	runStartupSweep()
	return &ToolSet{}, nil
}

// New builds a toolset without running the startup sweep, for tests and
// embedders that want predictable filesystem behaviour.
func New() *ToolSet {
	return &ToolSet{}
}

func (t *ToolSet) Instructions() string {
	return `## Plan Tools

Use this toolset to record the session's plan before executing it.

- ` + "`write_plan(content)`" + ` creates or replaces the plan for this session. There is exactly one plan per session and ` + "`content`" + ` is the full markdown — write_plan does not append.
- ` + "`read_plan()`" + ` returns the plan you (or an earlier turn) wrote. It errors when no plan has been written yet.
- ` + "`exit_plan_mode()`" + ` signals that the plan is ready for review. Call this once the plan is complete and you do not intend to change it on the next turn. It does not switch agents on its own — the host application decides what happens next based on the user's reply.`
}

type WritePlanArgs struct {
	Content string `json:"content" jsonschema:"The full plan content as markdown. Replaces the existing plan for this session."`
}

// Tools advertises the metadata only; Handler is intentionally nil so the
// runtime's toolMap takes over (same pattern as handoff and transfer_task).
func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:        ToolNameWritePlan,
			Category:    "plan",
			Description: "Create or replace the plan for this session as markdown. The plan path is derived from the session ID; there is exactly one plan per session.",
			Parameters:  tools.MustSchemaFor[WritePlanArgs](),
			Annotations: tools.ToolAnnotations{
				Title: "Write Plan",
			},
		},
		{
			Name:        ToolNameReadPlan,
			Category:    "plan",
			Description: "Read the plan written for this session and return it as markdown. Errors if no plan has been written yet.",
			Annotations: tools.ToolAnnotations{
				Title:        "Read Plan",
				ReadOnlyHint: true,
			},
		},
		{
			Name:        ToolNameExitPlanMode,
			Category:    "plan",
			Description: "Signal that the plan written for this session is ready for review. Only call this once the plan is complete and you do not intend to change it on the next turn. It does not switch agents.",
			Annotations: tools.ToolAnnotations{
				Title:        "Exit Plan Mode",
				ReadOnlyHint: true,
			},
		},
	}, nil
}

func Path(dir, sessionID string) (string, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return "", fmt.Errorf("%w: %q", ErrInvalidSessionID, sessionID)
	}
	return filepath.Join(dir, sessionID+".md"), nil
}

// WriteContent uses atomicfile.Write so a concurrent reader — in this process
// or another — sees either the old or the new file, never a partial one, and
// so an existing symlink is replaced rather than followed.
func WriteContent(dir, sessionID, content string) (string, error) {
	path, err := Path(dir, sessionID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create plans dir: %w", err)
	}
	if err := atomicfile.Write(path, bytes.NewReader([]byte(content)), 0o600); err != nil {
		return "", fmt.Errorf("write plan: %w", err)
	}
	return path, nil
}

// ReadContent returns ErrPlanNotFound (with the path so callers can include
// it in user-facing messages) on ENOENT, distinguishing "plan missing" from a
// real read failure such as a permission error.
func ReadContent(dir, sessionID string) (content, path string, err error) {
	path, err = Path(dir, sessionID)
	if err != nil {
		return "", "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", path, ErrPlanNotFound
	}
	if err != nil {
		return "", path, fmt.Errorf("read plan: %w", err)
	}
	return string(data), path, nil
}

// Sweep is best-effort: a permission glitch on one file should not block
// cleaning the rest, but the first error encountered is returned so callers
// can surface it.
func Sweep(dir string, now time.Time, maxAge time.Duration) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("scan plans dir: %w", err)
	}
	cutoff := now.Add(-maxAge)
	var firstErr error
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// runStartupSweep swallows the sweep error so a read-only data dir does not
// block the toolset from being created.
var runStartupSweep = sync.OnceFunc(func() {
	if err := Sweep(DefaultDir(), time.Now(), maxPlanAge); err != nil {
		slog.Warn("plan: sweep of stale plan files failed", "error", err, "dir", DefaultDir())
	}
})
