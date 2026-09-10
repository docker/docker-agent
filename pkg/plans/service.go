package plans

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/atomicfile"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
)

type service struct {
	storage plan.Storage
}

var _ Service = (*service)(nil)

// NewService returns a Service over the given shared-plan storage. Pass
// plan.SharedStorage() to operate on the same store — and serialize on the
// same mutex — as the plan tools of agents running in this process; any other
// plan.Storage yields an isolated service. The storage must not be nil.
func NewService(storage plan.Storage) Service {
	if storage == nil {
		panic("plans: storage must not be nil")
	}
	return &service{storage: storage}
}

func (s *service) List(ctx context.Context) (ListResult, error) {
	// Always a non-nil slice so an empty listing serializes as [] not null.
	result := ListResult{Plans: []Plan{}}

	summaries, warnings, err := s.storage.List(ctx)
	if err != nil {
		return ListResult{}, &StorageError{Scope: ScopeShared, Op: "list", Err: err}
	}
	result.Warnings = append(result.Warnings, warnings...)
	// The documented order is by name; enforce it here so it holds for any
	// injected Storage, not only backends that happen to sort.
	slices.SortStableFunc(summaries, func(a, b plan.Summary) int { return cmp.Compare(a.Name, b.Name) })
	for _, sum := range summaries {
		result.Plans = append(result.Plans, Plan{
			Scope:     ScopeShared,
			Name:      sum.Name,
			Title:     sum.Title,
			Author:    sum.Author,
			Status:    sum.Status,
			Version:   new(sum.Revision),
			UpdatedAt: parseUpdatedAt(sum.UpdatedAt),
		})
	}
	return result, nil
}

func (s *service) Get(ctx context.Context, ref Ref) (Plan, error) {
	if err := checkRef(ref); err != nil {
		return Plan{}, err
	}
	p, ok, err := s.storage.Get(ctx, ref.Name)
	if err != nil {
		return Plan{}, sharedError("get", ref.Name, err)
	}
	if !ok {
		return Plan{}, &NotFoundError{Scope: ScopeShared, Name: ref.Name}
	}
	return sharedPlan(p), nil
}

func (s *service) Create(ctx context.Context, req CreateRequest) (Plan, error) {
	if err := checkRef(req.Ref); err != nil {
		return Plan{}, err
	}
	if err := validateContent(req.Content); err != nil {
		return Plan{}, err
	}
	// MustNotExist makes the write create-only by existence, not by
	// revision: a plan that already exists conflicts even when its stored
	// revision is 0 (a hand-written or foreign file that omits the field).
	// ExpectedRevision 0 is kept alongside it as a defensive guard for
	// injected backends that predate MustNotExist: those still conflict for
	// any plan that ever took a revision bump.
	p, err := s.storage.Upsert(ctx, plan.UpsertRequest{
		Name:             req.Ref.Name,
		Content:          &req.Content,
		Title:            &req.Title,
		Author:           &req.Author,
		Status:           &req.Status,
		ExpectedRevision: new(0),
		MustNotExist:     true,
	})
	if err != nil {
		return Plan{}, sharedError("create", req.Ref.Name, err)
	}
	return sharedPlan(p), nil
}

func (s *service) Update(ctx context.Context, req UpdateRequest) (Plan, error) {
	if err := checkRef(req.Ref); err != nil {
		return Plan{}, err
	}
	if err := validateContent(req.Content); err != nil {
		return Plan{}, err
	}
	p, err := s.storage.Upsert(ctx, plan.UpsertRequest{
		Name:             req.Ref.Name,
		Content:          &req.Content,
		Title:            req.Title,
		Author:           req.Author,
		Status:           req.Status,
		ExpectedRevision: req.ExpectedVersion,
		MustExist:        true,
	})
	if err != nil {
		return Plan{}, sharedError("update", req.Ref.Name, err)
	}
	return sharedPlan(p), nil
}

func (s *service) SetStatus(ctx context.Context, req SetStatusRequest) (Plan, error) {
	if err := checkRef(req.Ref); err != nil {
		return Plan{}, err
	}
	if req.Status == "" {
		return Plan{}, &ValidationError{Message: "status must not be empty"}
	}
	p, err := s.storage.Upsert(ctx, plan.UpsertRequest{
		Name:             req.Ref.Name,
		Status:           &req.Status,
		ExpectedRevision: req.ExpectedVersion,
		MustExist:        true,
	})
	if err != nil {
		return Plan{}, sharedError("set_status", req.Ref.Name, err)
	}
	return sharedPlan(p), nil
}

func (s *service) Delete(ctx context.Context, req DeleteRequest) error {
	if err := checkRef(req.Ref); err != nil {
		return err
	}
	deleted, err := s.storage.Delete(ctx, req.Ref.Name, req.ExpectedVersion)
	if err != nil {
		return sharedError("delete", req.Ref.Name, err)
	}
	if !deleted {
		return &NotFoundError{Scope: ScopeShared, Name: req.Ref.Name}
	}
	return nil
}

func (s *service) Export(ctx context.Context, req ExportRequest) (ExportResult, error) {
	if req.Path == "" {
		return ExportResult{}, &ValidationError{Message: "path must not be empty"}
	}
	p, err := s.Get(ctx, req.Ref)
	if err != nil {
		return ExportResult{}, err
	}
	if err := writeExportFile(p.Scope, req.Path, p.Content, req.Force); err != nil {
		return ExportResult{}, err
	}
	return ExportResult{
		Scope:        p.Scope,
		Name:         p.Name,
		Path:         req.Path,
		Version:      p.Version,
		BytesWritten: len(p.Content),
	}, nil
}

// validateContent gates mutation content: it must be non-empty and within
// the advertised content cap, refused as invalid input before the storage is
// touched. Content of exactly the cap is accepted.
func validateContent(content string) error {
	if content == "" {
		return &ValidationError{Message: "content must not be empty"}
	}
	if len(content) > plan.MaxPlanContentSize {
		return &ValidationError{Message: fmt.Sprintf("content exceeds the maximum plan size (%d bytes; max %d)", len(content), plan.MaxPlanContentSize)}
	}
	return nil
}

// checkRef gates every operation: unknown scopes are invalid input, and names
// are validated with the storage's canonical rule before touching it.
func checkRef(ref Ref) error {
	if ref.Scope != ScopeShared {
		return invalidScopeError(ref.Scope)
	}
	if err := plan.ValidateName(ref.Name); err != nil {
		return &ValidationError{Message: err.Error()}
	}
	return nil
}

// sharedError maps a plan.Storage failure to this package's typed errors by
// the storage contract's own types, never by matching error text.
func sharedError(op, name string, err error) error {
	var conflict *plan.VersionConflictError
	if errors.As(err, &conflict) {
		return &ConflictError{Name: conflict.Name, Expected: conflict.Expected, Current: conflict.Current}
	}
	var corrupt *plan.CorruptPlanError
	if errors.As(err, &corrupt) {
		return &CorruptError{Scope: ScopeShared, Name: name, Err: err}
	}
	if errors.Is(err, plan.ErrPlanNotFound) {
		return &NotFoundError{Scope: ScopeShared, Name: name}
	}
	return &StorageError{Scope: ScopeShared, Op: op, Err: err}
}

func invalidScopeError(scope Scope) error {
	return &ValidationError{Message: fmt.Sprintf("invalid plan scope %q: use %q", scope, ScopeShared)}
}

func sharedPlan(p plan.Plan) Plan {
	return Plan{
		Scope:     ScopeShared,
		Name:      p.Name,
		Title:     p.Title,
		Author:    p.Author,
		Status:    p.Status,
		Content:   p.Content,
		Version:   new(p.Revision),
		UpdatedAt: parseUpdatedAt(p.UpdatedAt),
	}
}

// parseUpdatedAt tolerates a missing or malformed stored timestamp: it is
// display metadata, so it degrades to the zero time instead of failing a read.
func parseUpdatedAt(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// writeExportFile mirrors the plan toolset's export behaviour — parent
// directories are created and a reader never observes a partial body —
// hardened with an overwrite policy. Without force the fully written content
// is published by hard-linking a temp file into place (see
// publishExportNoReplace): the link atomically refuses any existing
// destination entry, so two racing non-force exports cannot clobber each
// other and the destination only ever holds a complete export; a filesystem
// without hard-link support surfaces that as a *StorageError. With force an
// existing regular file is replaced atomically by rename; directories and
// non-regular files are refused either way.
func writeExportFile(scope Scope, path, content string, force bool) error {
	clean := filepath.Clean(path)
	if info, err := os.Stat(clean); err == nil {
		switch {
		case info.IsDir():
			return &ValidationError{Message: fmt.Sprintf("path %q is a directory, not a file", path)}
		case !info.Mode().IsRegular():
			return &ValidationError{Message: fmt.Sprintf("path %q exists and is not a regular file", path)}
		case !force:
			return exportExistsError(path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(clean), 0o700); err != nil {
		return &StorageError{Scope: scope, Op: "export", Err: err}
	}
	if !force {
		if err := publishExportNoReplace(clean, content); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return exportExistsError(path)
			}
			return &StorageError{Scope: scope, Op: "export", Err: err}
		}
		return nil
	}
	if err := atomicfile.Write(clean, strings.NewReader(content), 0o600); err != nil {
		return &StorageError{Scope: scope, Op: "export", Err: err}
	}
	return nil
}

// publishExportNoReplace publishes content at dest without replacing
// anything: the body is fully written, synced, and closed in a temp file
// next to dest, then published with os.Link, which fails with fs.ErrExist
// when any destination entry exists and otherwise exposes the complete inode
// in a single step — a reader can never observe a partial or empty export.
// The temp link is removed afterward; a successful publication leaves dest
// as the inode's surviving name.
func publishExportNoReplace(dest, content string) error {
	f, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// CreateTemp's 0o600 is subject to the umask; pin the mode exactly so
	// non-force and force exports publish identical permissions.
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Link(tmp, dest)
}

func exportExistsError(path string) error {
	return &ValidationError{Message: fmt.Sprintf("path %q already exists; use force to replace it", path)}
}
