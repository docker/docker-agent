package plans

import "fmt"

// NotFoundError reports that the addressed plan does not exist in its scope.
type NotFoundError struct {
	Scope Scope
	Name  string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s plan %q not found", e.Scope, e.Name)
}

// ValidationError reports invalid caller input: a malformed plan name, an
// unknown scope, empty or oversized content, or an empty status.
type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

// CorruptError reports a plan that exists but cannot be read or decoded, so a
// caller never mistakes a damaged plan for a missing one and recreates it.
type CorruptError struct {
	Scope Scope
	Name  string
	Err   error
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("%s plan %q: %v", e.Scope, e.Name, e.Err)
}

func (e *CorruptError) Unwrap() error { return e.Err }

// StorageError reports a backend failure (I/O, permissions, ...) that is
// neither a missing nor a corrupt plan.
type StorageError struct {
	Scope Scope
	Op    string
	Err   error
}

func (e *StorageError) Error() string {
	return fmt.Sprintf("%s plan storage failure during %s: %v", e.Scope, e.Op, e.Err)
}

func (e *StorageError) Unwrap() error { return e.Err }

// ConflictError reports a mutation rejected because the caller's expected
// version no longer matches the plan's current one. Current carries the
// version the plan is actually at, so a frontend can offer "re-read and
// retry" or a deliberate force. Expected 0 identifies a failed create: the
// plan already exists.
type ConflictError struct {
	Name     string
	Expected int
	Current  int
}

func (e *ConflictError) Error() string {
	// A create (expected version 0) cannot be forced or retried against a
	// newer version; the way out is a different name or an update.
	if e.Expected == 0 {
		return fmt.Sprintf("plan %q already exists (current version %d); pick a different name, or update the existing plan", e.Name, e.Current)
	}
	return fmt.Sprintf("version conflict on plan %q: expected version %d does not match current version %d; re-read the plan and retry, or force to overwrite", e.Name, e.Expected, e.Current)
}
