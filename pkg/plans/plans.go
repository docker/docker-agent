// Package plans is the host-facing contract for managing plans from a
// frontend such as a CLI or TUI. It wraps the shared, named plans agents
// collaborate on (pkg/tools/builtin/plan) behind one model and one Service.
//
// The package wraps the existing storage rather than duplicating it. Shared
// plans go through a caller-supplied plan.Storage — pass plan.SharedStorage()
// to operate on the same store, and thus the same mutex, as the plan tools of
// agents running in this process.
package plans

import (
	"context"
	"time"
)

// Scope identifies which plan system a plan belongs to. It remains part of
// the wire contract for forward compatibility even though only one scope
// exists today.
type Scope string

// ScopeShared is the cross-session store of named plans that agents
// collaborate on. Shared plans are versioned and fully mutable.
const ScopeShared Scope = "shared"

// Plan is the host-facing view of a plan.
//
// The JSON tags are a stable, snake_case wire contract for host consumers
// (e.g. the plans CLI --json output). It is deliberately independent of the
// agent tool JSON of pkg/tools/builtin/plan, which keeps its historical
// camelCase fields (updatedAt, bytesWritten) for backward compatibility.
type Plan struct {
	// Scope tells which plan system the plan lives in.
	Scope Scope `json:"scope"`
	// Name is the validated canonical plan name.
	Name string `json:"name"`
	// Title, Author, and Status are plan metadata. Status is a free-form
	// lifecycle label with no fixed vocabulary.
	Title  string `json:"title,omitempty"`
	Author string `json:"author,omitempty"`
	Status string `json:"status,omitempty"`
	// Content is the plan body. List returns metadata only, so Content is
	// empty there; Get populates it.
	Content string `json:"content,omitempty"`
	// Version is the optimistic-lock revision of the plan.
	Version *int `json:"version,omitempty"`
	// UpdatedAt is the stored time of the last write. Zero when unknown (and
	// then omitted from JSON via omitzero, which consults time.Time.IsZero;
	// omitempty would keep the zero struct).
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// Ref addresses a plan by name within a scope.
type Ref struct {
	Scope Scope
	Name  string
}

// SharedRef addresses the named shared plan.
func SharedRef(name string) Ref { return Ref{Scope: ScopeShared, Name: name} }

// ListResult is the outcome of List. Warnings carries plans that exist but
// could not be read, so a caller can tell "no plans" apart from "some plans
// failed to load".
type ListResult struct {
	Plans    []Plan   `json:"plans"`
	Warnings []string `json:"warnings,omitempty"`
}

// CreateRequest creates a new shared plan. Create is create-only: the write
// is guarded by expected version 0, so it fails with a *ConflictError instead
// of silently overwriting a plan that already exists.
type CreateRequest struct {
	Ref     Ref
	Content string
	Title   string
	Author  string
	Status  string
}

// UpdateRequest replaces the content of an existing shared plan. Nil metadata
// pointers preserve the previous values; explicit values (including "")
// overwrite them.
//
// ExpectedVersion is the version the caller last read: the write is rejected
// with a *ConflictError when it no longer matches. An explicit nil means
// unconditional replacement and must only be sent when the user deliberately
// chose to force.
type UpdateRequest struct {
	Ref             Ref
	Content         string
	Title           *string
	Author          *string
	Status          *string
	ExpectedVersion *int
}

// SetStatusRequest sets the free-form status of an existing shared plan
// without touching its body. ExpectedVersion follows the same rules as in
// UpdateRequest.
type SetStatusRequest struct {
	Ref             Ref
	Status          string
	ExpectedVersion *int
}

// DeleteRequest removes a shared plan. ExpectedVersion follows the same rules
// as in UpdateRequest: nil deletes unconditionally.
type DeleteRequest struct {
	Ref             Ref
	ExpectedVersion *int
}

// ExportRequest writes a plan's content to a file on disk.
//
// Force replaces an existing regular file at Path. Without it, Export
// refuses any existing destination with a *ValidationError and leaves it
// untouched.
type ExportRequest struct {
	Ref   Ref
	Path  string
	Force bool
}

// ExportResult reports a completed export. Version is the exported plan's
// version.
type ExportResult struct {
	Scope        Scope  `json:"scope"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	Version      *int   `json:"version,omitempty"`
	BytesWritten int    `json:"bytes_written"`
}

// Service is the host-facing contract for managing plans. Failures are
// reported as the typed errors of this package so frontends never classify
// by error text.
type Service interface {
	// List returns plan metadata (Content is left empty) for every shared
	// plan, sorted by name.
	List(ctx context.Context) (ListResult, error)
	// Get returns the full plan, including content. A missing plan is a
	// *NotFoundError.
	Get(ctx context.Context, ref Ref) (Plan, error)
	// Create adds a new shared plan; a name that already exists fails with a
	// *ConflictError carrying the current version.
	Create(ctx context.Context, req CreateRequest) (Plan, error)
	// Update replaces the content (and optionally metadata) of an existing
	// shared plan, honouring req.ExpectedVersion.
	Update(ctx context.Context, req UpdateRequest) (Plan, error)
	// SetStatus sets the free-form status of an existing shared plan,
	// honouring req.ExpectedVersion.
	SetStatus(ctx context.Context, req SetStatusRequest) (Plan, error)
	// Delete removes a shared plan, honouring req.ExpectedVersion. Deleting a
	// missing plan is a *NotFoundError.
	Delete(ctx context.Context, req DeleteRequest) error
	// Export writes a plan's content to req.Path, creating parent directories
	// as needed. An existing destination is refused with a *ValidationError
	// and preserved unless req.Force is set, which replaces an existing
	// regular file atomically; directories and non-regular files are always
	// refused.
	Export(ctx context.Context, req ExportRequest) (ExportResult, error)
}
