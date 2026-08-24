// Package plan provides a toolset that lets two or more agents collaborate on
// plans addressed by name. Plans persist through a pluggable Storage backend;
// the default FilesystemStorage keeps them as JSON files in a global shared
// folder under the docker-agent data directory, so any agent that loads this
// toolset can write, read, list, and delete the same shared plans, and they
// persist across sessions. Embedders can inject an alternative backend (e.g. a
// database or remote store) with WithStorage.
//
// Concurrency: agents that share one ToolSet instance also share its Storage,
// which serializes their operations. The default FilesystemStorage guards its
// read-modify-write revision bump with a mutex within the process and with an
// exclusive advisory lock on a sentinel file in the plans directory across
// processes, and writes atomically (write-to-temp + rename). A reader —
// including a separate docker-agent process — never observes a partially
// written plan, and two processes writing the *same* plan at the very same
// instant cannot both pass an optimistic-lock check: exactly one wins and the
// other gets a deterministic *VersionConflictError. Other backends own the
// same guarantees through their Upsert/Delete implementations.
package plan

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/atomicfile"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameWritePlan          = "write_plan"
	ToolNameReadPlan           = "read_plan"
	ToolNameListPlans          = "list_plans"
	ToolNameDeletePlan         = "delete_plan"
	ToolNameUpdatePlanFromFile = "update_plan_from_file"
	ToolNameExportPlanToFile   = "export_plan_to_file"
	ToolNameSetPlanStatus      = "set_plan_status"
	ToolNameGetPlanStatus      = "get_plan_status"
)

// MaxPlanContentSize caps a plan's content (its markdown body): it bounds how
// much update_plan_from_file will read from disk, so a pathological or wrong
// path cannot make the agent pull an arbitrarily large file into a plan (and
// into the model's context), and FilesystemStorage refuses to persist content
// beyond it. 10 MiB is far above any realistic plan while still bounding
// memory, and content of exactly this size is accepted. It is exported so
// host-side callers (e.g. the docker agent plans CLI) bound plan content with
// the same limit instead of duplicating the number.
const MaxPlanContentSize = 10 << 20

// MaxPlanFileSize is the name MaxPlanContentSize was first exported under.
//
// Deprecated: use MaxPlanContentSize. Kept as an alias so external callers
// built against the original export keep compiling and share the same bound.
const MaxPlanFileSize = MaxPlanContentSize

// maxEncodedPlanSize bounds a stored plan file on the way back in: load
// refuses to decode anything beyond it, so a corrupt or foreign oversized
// file in the plans directory cannot cause unbounded allocation. It is
// deliberately larger than MaxPlanContentSize: JSON escaping expands a
// content byte to at most 6 bytes (\u00XX), so content of exactly the cap
// encodes to at most 6*MaxPlanContentSize. The additional 10 MiB budgets
// the free-form metadata (title, author, status), the fixed fields, and the
// envelope. Metadata deliberately has no per-field cap — labels are
// free-form and plans stored by earlier builds must keep loading — so this
// budget is at least the headroom metadata effectively had under the
// previous 10 MiB whole-file bound; save re-checks the encoded size, so
// only pathological aggregate metadata is ever refused, as a storage limit.
const maxEncodedPlanSize = 6*MaxPlanContentSize + 10<<20

// ErrPlanNotFound is returned by write operations that require an existing plan
// (set_plan_status) when the named plan does not exist, so callers can tell a
// missing plan apart from a real backend failure.
var ErrPlanNotFound = errors.New("plan not found")

// VersionConflictError is returned by a write when the caller's
// last_known_revision does not match the plan's current revision, signalling
// that another writer changed the plan in the meantime. The caller should
// re-read the plan and retry against the new revision.
type VersionConflictError struct {
	Name     string
	Expected int
	Current  int
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("version conflict on plan %q: last_known_revision %d does not match current revision %d; re-read the plan and retry", e.Name, e.Expected, e.Current)
}

// CorruptPlanError is returned when a plan file exists but cannot be decoded,
// so callers can tell a damaged plan apart from a missing one or a plain I/O
// failure without matching on error text.
type CorruptPlanError struct {
	// File is the base name of the plan file that failed to decode.
	File string
	Err  error
}

func (e *CorruptPlanError) Error() string {
	return fmt.Sprintf("plan file %s is corrupt: %v", e.File, e.Err)
}

func (e *CorruptPlanError) Unwrap() error { return e.Err }

// namePattern defines the accepted plan-name format: a lowercase slug made of
// alphanumerics, '-' and '_'. Names are validated against it rather than being
// silently rewritten, so two different inputs can never collapse onto the same
// file (which would let one plan clobber another) and no input can escape the
// plans directory via path separators or "..".
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ValidateName rejects names that do not match namePattern. It is the
// canonical plan-name rule, exported so host-side callers (e.g. pkg/plans)
// validate exactly like the storage instead of duplicating the pattern.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid plan name %q: use only lowercase letters, digits, '-' and '_', starting with a letter or digit", name)
	}
	return nil
}

// Plan is a shared document collaborated on by the agents.
type Plan struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Content string `json:"content"`
	// Author is a free-form label identifying who last wrote the plan
	// (typically the agent name). It helps collaborators see who made the
	// most recent change.
	Author string `json:"author,omitempty"`
	// Status is a free-form lifecycle label (e.g. "idle", "in-progress",
	// "blocked", "done"). There is no fixed vocabulary: agents and users define
	// their own in the system prompt.
	Status string `json:"status,omitempty"`
	// Revision is the plan's monotonically increasing version number, bumped on
	// every write. Callers capture it on a read and pass it back as
	// last_known_revision to detect concurrent modifications (optimistic
	// locking).
	Revision  int    `json:"revision"`
	UpdatedAt string `json:"updatedAt"`
}

// Summary is a lightweight view of a plan returned by list_plans.
type Summary struct {
	Name      string `json:"name"`
	Title     string `json:"title,omitempty"`
	Author    string `json:"author,omitempty"`
	Status    string `json:"status,omitempty"`
	Revision  int    `json:"revision"`
	UpdatedAt string `json:"updatedAt"`
}

// StatusView is the lightweight result of set_plan_status and get_plan_status:
// just the status and the revision, without the plan body, so reading or
// writing the status never costs the tokens of the full content.
type StatusView struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Revision int    `json:"revision"`
}

// ExportResult is the result of export_plan_to_file. It deliberately omits the
// plan content: the content is written to disk, not returned, so materialising
// a plan on disk before an edit costs no output tokens.
type ExportResult struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	Title        string `json:"title,omitempty"`
	Status       string `json:"status,omitempty"`
	Revision     int    `json:"revision"`
	BytesWritten int    `json:"bytesWritten"`
}

// UpsertRequest describes a create-or-update operation on a plan. Content,
// Title, Author and Status are pointers so an omitted field (nil) preserves the
// previous value while an explicit value (including "") overwrites it; this lets
// a caller change just the status without rewriting the body. ExpectedRevision
// enables optimistic locking: when non-nil, the write is rejected with a
// *VersionConflictError unless it equals the plan's current revision. MustExist
// makes the write fail with ErrPlanNotFound when the plan does not already
// exist (used by set_plan_status, which must not create a plan). MustNotExist
// makes the write create-only by existence, not by revision: any existing
// plan is rejected with a *VersionConflictError carrying Expected 0 and the
// plan's current revision — including revision 0, which a stored plan can
// legitimately carry (a hand-written or foreign file that omits the field),
// so ExpectedRevision 0 alone is not a create-only guard.
type UpsertRequest struct {
	Name             string
	Content          *string
	Title            *string
	Author           *string
	Status           *string
	ExpectedRevision *int
	MustExist        bool
	MustNotExist     bool
}

// ListResult is the output of list_plans. Warnings lists plan files that could
// not be read or decoded, so a caller can tell "no plans exist" apart from
// "some plans failed to load" — important because an agent that mistakes a
// temporarily unreadable plan for a missing one could recreate and clobber it.
type ListResult struct {
	Plans    []Summary `json:"plans"`
	Warnings []string  `json:"warnings,omitempty"`
}

// Storage persists the plans a ToolSet operates on. Implementations decide how
// a plan is stored (files, memory, a database, a remote service) and own the
// revision bump in Upsert, so a backend can make it atomic. The default
// FilesystemStorage is used when WithStorage injects nothing else.
type Storage interface {
	// Get returns the named plan. The bool is false with a nil error when no
	// such plan exists, so callers can tell a missing plan apart from a real
	// read failure (returned as a non-nil error).
	Get(ctx context.Context, name string) (Plan, bool, error)
	// Upsert creates or updates a plan as described by req: nil fields are
	// preserved from the previous revision, the optimistic-lock check and the
	// existence guards are honoured atomically with the write, the revision is
	// bumped and UpdatedAt stamped. It returns a *VersionConflictError on a
	// revision mismatch or when req.MustNotExist is set but the plan exists
	// (Expected 0, Current the plan's revision — even revision 0), and
	// ErrPlanNotFound when req.MustExist is set but the plan is absent.
	Upsert(ctx context.Context, req UpsertRequest) (Plan, error)
	// List returns a summary of every stored plan. Warnings carries entries
	// that could not be read, so a caller can tell "no plans" apart from "some
	// plans failed to load".
	List(ctx context.Context) (plans []Summary, warnings []string, err error)
	// Delete removes the named plan. The bool is false with a nil error when
	// there was no such plan to delete. When expectedRevision is non-nil the
	// delete is rejected with a *VersionConflictError unless it matches the
	// plan's current revision.
	Delete(ctx context.Context, name string, expectedRevision *int) (deleted bool, err error)
}

// Change action labels reported through ChangeNotifier subscriptions.
const (
	ChangeActionWrite  = "write"
	ChangeActionStatus = "status"
	ChangeActionDelete = "delete"
)

// Change describes one successful mutation of a shared plan. It carries
// identity and version only — never content — so observers refresh through
// their own storage view instead of trusting an event payload. Revision is
// the version after a write, or the guard version of a guarded delete (0
// when the delete was unguarded). It deliberately carries no mutator
// identity: the shared singleton toolset executes tool calls for whichever
// session's agent is running, so a subscriber must not attribute a change to
// its own agent. The plan's Author field is the collaborative attribution.
type Change struct {
	Name     string
	Action   string
	Revision int
}

// ChangeNotifier is the optional host-notification capability of the plan
// toolset: hosts (e.g. one runtime stream per TUI tab) subscribe callbacks
// that fire after every successful mutation and never for failed ones.
// Discover it through toolset wrappers with tools.As. Any number of
// subscribers may be active at once — every one receives every change —
// because the shared singleton toolset serves all sessions in the process.
// Callbacks must be safe for concurrent use and must not block; they do not
// alter tool schemas, outputs, or storage behavior in any way.
type ChangeNotifier interface {
	// SubscribeChanges registers cb and returns an idempotent unsubscribe
	// function that removes exactly this subscription.
	SubscribeChanges(cb func(Change)) (unsubscribe func())
}

type ToolSet struct {
	storage Storage

	// subsMu guards the subscriber registry. A mutex (not an atomic slot)
	// because the shared singleton toolset serves several sessions at once:
	// each active stream holds its own subscription, and subscriptions come
	// and go while other sessions' tool calls are notifying concurrently.
	subsMu      sync.Mutex
	subscribers map[uint64]func(Change)
	nextSubID   uint64
}

var (
	_ tools.ToolSet      = (*ToolSet)(nil)
	_ tools.Instructable = (*ToolSet)(nil)
	_ tools.Describer    = (*ToolSet)(nil)
	_ ChangeNotifier     = (*ToolSet)(nil)
)

// SubscribeChanges registers cb to run after every successful plan mutation
// and returns an unsubscribe function. Every active subscriber receives every
// change exactly once; unsubscribing removes only that subscription and is
// idempotent. A nil cb registers nothing and returns a no-op unsubscribe.
// Implements ChangeNotifier.
func (t *ToolSet) SubscribeChanges(cb func(Change)) func() {
	if cb == nil {
		return func() {}
	}

	t.subsMu.Lock()
	defer t.subsMu.Unlock()
	if t.subscribers == nil {
		t.subscribers = make(map[uint64]func(Change))
	}
	id := t.nextSubID
	t.nextSubID++
	t.subscribers[id] = cb
	return func() {
		t.subsMu.Lock()
		defer t.subsMu.Unlock()
		// Deleting a missing key is a no-op, which makes unsubscribe
		// idempotent and safe to call from multiple teardown paths.
		delete(t.subscribers, id)
	}
}

// notifyChange fans change out to all current subscribers. The subscriber
// set is copied under the lock and invoked outside it, so a callback can
// subscribe or unsubscribe without deadlocking the registry and a slow
// callback never blocks concurrent subscription changes.
func (t *ToolSet) notifyChange(change Change) {
	t.subsMu.Lock()
	subs := make([]func(Change), 0, len(t.subscribers))
	for _, cb := range t.subscribers {
		subs = append(subs, cb)
	}
	t.subsMu.Unlock()

	for _, cb := range subs {
		cb(change)
	}
}

// Option configures a ToolSet.
type Option func(*ToolSet)

// WithStorage injects a custom Storage backend in place of the default
// FilesystemStorage, letting embedders supply their own store and get
// per-instance isolation. The provided storage must not be nil.
func WithStorage(storage Storage) Option {
	if storage == nil {
		panic("plan: storage must not be nil")
	}
	return func(t *ToolSet) {
		t.storage = storage
	}
}

// sharedToolSet returns the one ToolSet shared by every agent in this process,
// built once on first use. Sharing a single instance means all collaborating
// agents serialize their plan operations on the same storage.
//
// Building the struct cannot fail, so the directory is *not* created here:
// FilesystemStorage runs os.MkdirAll on every write, which means a directory
// that is momentarily unavailable at startup (e.g. a not-yet-mounted parent) is
// recovered from automatically instead of being permanently memoized as an
// error.
var sharedToolSet = sync.OnceValue(func() *ToolSet {
	return New()
})

// CreateToolSet is used by the tools registry. It returns a process-wide
// singleton so that all agents collaborating in the same process share one
// storage over the global plans folder.
func CreateToolSet() (tools.ToolSet, error) {
	return sharedToolSet(), nil
}

// SharedStorage returns the Storage behind the process-wide shared ToolSet
// (CreateToolSet). Host code that manages plans outside agent tool calls
// (e.g. a TUI) should use it so both sides serialize on the same instance
// and mutex. ToolSets built with New keep their own independent storage.
func SharedStorage() Storage {
	return sharedToolSet().storage
}

// DefaultDir is the global shared folder where plans are stored, under the
// docker-agent data directory.
func DefaultDir() string {
	return filepath.Join(paths.GetDataDir(), "plans")
}

// New builds a per-instance plan toolset. With no options it uses the default
// FilesystemStorage rooted at DefaultDir(); pass WithStorage to inject another
// backend. Each call returns an independent instance — use CreateToolSet for
// the process-wide singleton shared by collaborating agents.
func New(opts ...Option) *ToolSet {
	t := &ToolSet{
		storage: NewFilesystemStorage(DefaultDir()),
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

func (t *ToolSet) Describe() string {
	if s, ok := t.storage.(fmt.Stringer); ok {
		return "plan(" + s.String() + ")"
	}
	return "plan"
}

func (t *ToolSet) Instructions() string {
	return `## Plan Tools

Collaborate on shared, named plans with other agents. Plans are stored in a
global shared folder, so every agent using this toolset sees the same plans.

- Use list_plans to discover existing plans.
- Use read_plan to inspect a plan before acting on or changing it.
- Use write_plan to create or update a plan by name. Writing replaces the whole
  document, so read it first and preserve any content you want to keep. Set the
  title and author (your agent name) so collaborators can see who made the
  latest change. Each write bumps the revision number.
- Use delete_plan to remove a plan once it is no longer needed.

### Editing a large plan cheaply

Re-sending the whole plan body on every edit is expensive. To avoid it:

1. Call export_plan_to_file to write the current plan content to a file. The
   content is written to disk and is NOT returned, so this costs no tokens.
2. Edit that file in place with your filesystem tools.
3. Call update_plan_from_file with the same path to commit the new content.

### Status

Each plan has a free-form status string (you define the vocabulary, e.g.
"idle", "in-progress", "blocked", "done"). Use set_plan_status and
get_plan_status to write and read it independently of the body, or pass status
to write_plan / update_plan_from_file.

### Concurrent edits (optimistic locking)

Reads return a revision number. When several sessions edit one plan, pass the
revision you last read as last_known_revision to write_plan, update_plan_from_file,
set_plan_status or delete_plan. If the plan changed in the meantime the write is
rejected with a version-conflict error: re-read the plan and retry.

Plan names must be lowercase and may contain only letters, digits, '-' and '_'
(for example "release-2025" or "db_migration").`
}

// optStr maps an omitted (empty) tool argument to a nil pointer, so an UpsertRequest
// preserves the previous value rather than overwriting it with "". A non-empty
// value is passed through as an explicit set.
func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type WritePlanArgs struct {
	Name              string `json:"name" jsonschema:"The plan name. Lowercase letters, digits, '-' and '_' only (e.g. 'release', 'db-migration')."`
	Content           string `json:"content" jsonschema:"The full plan content (markdown). Replaces the existing plan."`
	Title             string `json:"title,omitempty" jsonschema:"Optional human-readable title. Preserved from the previous revision when omitted."`
	Author            string `json:"author,omitempty" jsonschema:"Optional label identifying who is writing the plan (typically the agent name). Preserved from the previous revision when omitted."`
	Status            string `json:"status,omitempty" jsonschema:"Optional free-form lifecycle status (e.g. 'in-progress', 'blocked', 'done'). Preserved from the previous revision when omitted."`
	LastKnownRevision *int   `json:"last_known_revision,omitempty" jsonschema:"Optional optimistic-lock guard. When set, the write is rejected with a conflict error unless it matches the plan's current revision. Obtain it by reading the plan first."`
}

func (t *ToolSet) writePlan(ctx context.Context, params WritePlanArgs) (*tools.ToolCallResult, error) {
	if params.Content == "" {
		return tools.ResultError("content must not be empty"), nil
	}

	plan, err := t.storage.Upsert(ctx, UpsertRequest{
		Name:             params.Name,
		Content:          &params.Content,
		Title:            optStr(params.Title),
		Author:           optStr(params.Author),
		Status:           optStr(params.Status),
		ExpectedRevision: params.LastKnownRevision,
	})
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	t.notifyChange(Change{Name: plan.Name, Action: ChangeActionWrite, Revision: plan.Revision})
	return tools.ResultJSON(plan), nil
}

type UpdatePlanFromFileArgs struct {
	Name              string `json:"name" jsonschema:"The plan name. Lowercase letters, digits, '-' and '_' only."`
	Path              string `json:"path" jsonschema:"Path to a file whose contents become the new plan content. Lets you commit edits without re-sending the full plan body."`
	Title             string `json:"title,omitempty" jsonschema:"Optional human-readable title. Preserved from the previous revision when omitted."`
	Author            string `json:"author,omitempty" jsonschema:"Optional label identifying who is writing the plan. Preserved from the previous revision when omitted."`
	Status            string `json:"status,omitempty" jsonschema:"Optional free-form lifecycle status. Preserved from the previous revision when omitted."`
	LastKnownRevision *int   `json:"last_known_revision,omitempty" jsonschema:"Optional optimistic-lock guard. When set, the write is rejected with a conflict error unless it matches the plan's current revision."`
}

func (t *ToolSet) updatePlanFromFile(ctx context.Context, params UpdatePlanFromFileArgs) (*tools.ToolCallResult, error) {
	if params.Path == "" {
		return tools.ResultError("path must not be empty"), nil
	}

	content, err := readPlanFile(params.Path)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	if content == "" {
		return tools.ResultError("file is empty; plan content must not be empty"), nil
	}

	plan, err := t.storage.Upsert(ctx, UpsertRequest{
		Name:             params.Name,
		Content:          &content,
		Title:            optStr(params.Title),
		Author:           optStr(params.Author),
		Status:           optStr(params.Status),
		ExpectedRevision: params.LastKnownRevision,
	})
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	t.notifyChange(Change{Name: plan.Name, Action: ChangeActionWrite, Revision: plan.Revision})
	return tools.ResultJSON(plan), nil
}

type ExportPlanToFileArgs struct {
	Name string `json:"name" jsonschema:"The name of the plan to export."`
	Path string `json:"path" jsonschema:"Path to write the plan content to. The content is written to disk and is NOT returned as tool output, so you can materialise the plan cheaply before editing it."`
}

func (t *ToolSet) exportPlanToFile(ctx context.Context, params ExportPlanToFileArgs) (*tools.ToolCallResult, error) {
	if params.Path == "" {
		return tools.ResultError("path must not be empty"), nil
	}

	plan, ok, err := t.storage.Get(ctx, params.Name)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	if !ok {
		return tools.ResultError(fmt.Sprintf("plan %q not found", params.Name)), nil
	}

	if err := writePlanFile(params.Path, plan.Content); err != nil {
		return tools.ResultError(err.Error()), nil
	}

	return tools.ResultJSON(ExportResult{
		Name:         params.Name,
		Path:         params.Path,
		Title:        plan.Title,
		Status:       plan.Status,
		Revision:     plan.Revision,
		BytesWritten: len(plan.Content),
	}), nil
}

type SetPlanStatusArgs struct {
	Name              string `json:"name" jsonschema:"The name of the plan whose status to set."`
	Status            string `json:"status" jsonschema:"The new free-form status string (e.g. 'in-progress', 'blocked', 'done'). Defined by your own vocabulary."`
	LastKnownRevision *int   `json:"last_known_revision,omitempty" jsonschema:"Optional optimistic-lock guard. When set, the write is rejected with a conflict error unless it matches the plan's current revision."`
}

func (t *ToolSet) setPlanStatus(ctx context.Context, params SetPlanStatusArgs) (*tools.ToolCallResult, error) {
	if params.Status == "" {
		return tools.ResultError("status must not be empty"), nil
	}

	plan, err := t.storage.Upsert(ctx, UpsertRequest{
		Name:             params.Name,
		Status:           &params.Status,
		MustExist:        true,
		ExpectedRevision: params.LastKnownRevision,
	})
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}

	t.notifyChange(Change{Name: plan.Name, Action: ChangeActionStatus, Revision: plan.Revision})
	return tools.ResultJSON(StatusView{Name: plan.Name, Status: plan.Status, Revision: plan.Revision}), nil
}

type GetPlanStatusArgs struct {
	Name string `json:"name" jsonschema:"The name of the plan whose status to read."`
}

func (t *ToolSet) getPlanStatus(ctx context.Context, params GetPlanStatusArgs) (*tools.ToolCallResult, error) {
	plan, ok, err := t.storage.Get(ctx, params.Name)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	if !ok {
		return tools.ResultError(fmt.Sprintf("plan %q not found", params.Name)), nil
	}

	return tools.ResultJSON(StatusView{Name: params.Name, Status: plan.Status, Revision: plan.Revision}), nil
}

type ReadPlanArgs struct {
	Name string `json:"name" jsonschema:"The name of the plan to read."`
}

func (t *ToolSet) readPlan(ctx context.Context, params ReadPlanArgs) (*tools.ToolCallResult, error) {
	plan, ok, err := t.storage.Get(ctx, params.Name)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	if !ok {
		return tools.ResultError(fmt.Sprintf("plan %q not found", params.Name)), nil
	}

	return tools.ResultJSON(plan), nil
}

func (t *ToolSet) listPlans(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
	plans, warnings, err := t.storage.List(ctx)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	// Always emit a non-nil slice so the output is "plans":[] rather than
	// "plans":null, regardless of what a backend returns when empty.
	if plans == nil {
		plans = []Summary{}
	}

	return tools.ResultJSON(ListResult{Plans: plans, Warnings: warnings}), nil
}

type DeletePlanArgs struct {
	Name              string `json:"name" jsonschema:"The name of the plan to delete."`
	LastKnownRevision *int   `json:"last_known_revision,omitempty" jsonschema:"Optional optimistic-lock guard. When set, the delete is rejected with a conflict error unless it matches the plan's current revision."`
}

func (t *ToolSet) deletePlan(ctx context.Context, params DeletePlanArgs) (*tools.ToolCallResult, error) {
	deleted, err := t.storage.Delete(ctx, params.Name, params.LastKnownRevision)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	if !deleted {
		return tools.ResultError(fmt.Sprintf("plan %q not found", params.Name)), nil
	}

	revision := 0
	if params.LastKnownRevision != nil {
		revision = *params.LastKnownRevision
	}
	t.notifyChange(Change{Name: params.Name, Action: ChangeActionDelete, Revision: revision})
	return tools.ResultJSON(map[string]string{"deleted": params.Name}), nil
}

// readPlanFile reads plan content from a file for update_plan_from_file. It
// rejects directories, non-regular files, and oversized files with a clear
// message instead of pulling unexpected data into a plan. Every check runs on
// the opened descriptor (File.Stat), never on the path, so the file that is
// validated is exactly the file that is read and a concurrent path swap
// cannot slip a directory, device, or named pipe past the checks. The open
// itself is hang-safe for those cases too (see OpenContentFile), and a
// device like /dev/zero, which would stream unbounded data, is rejected
// before any read. The read goes through an io.LimitReader rather than
// trusting the stat size, which closes the race where the file grows between
// the size check and the read.
func readPlanFile(path string) (string, error) {
	f, err := OpenContentFile(filepath.Clean(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("file %q does not exist", path)
		}
		return "", fmt.Errorf("reading plan file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("reading plan file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("path %q is a directory, not a file", path)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("path %q is not a regular file (e.g. a device or named pipe)", path)
	}
	if info.Size() > MaxPlanContentSize {
		return "", fmt.Errorf("file %q is too large (%d bytes; max %d)", path, info.Size(), MaxPlanContentSize)
	}

	// Read one byte past the cap so an over-cap file is detected even if it grew
	// since the size check above.
	data, err := io.ReadAll(io.LimitReader(f, MaxPlanContentSize+1))
	if err != nil {
		return "", fmt.Errorf("reading plan file: %w", err)
	}
	if len(data) > MaxPlanContentSize {
		return "", fmt.Errorf("file %q is too large (max %d bytes)", path, MaxPlanContentSize)
	}
	return string(data), nil
}

// writePlanFile writes plan content to a file for export_plan_to_file. It
// creates any missing parent directories and writes atomically (temp + rename),
// so a reader never sees a partial export and the agent can point at a fresh
// path without first creating its directory.
func writePlanFile(path, content string) error {
	clean := filepath.Clean(path)

	if info, err := os.Stat(clean); err == nil && info.IsDir() {
		return fmt.Errorf("path %q is a directory, not a file", path)
	}
	if dir := filepath.Dir(clean); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating directory for plan file: %w", err)
		}
	}
	if err := atomicfile.Write(clean, strings.NewReader(content), 0o600); err != nil {
		return fmt.Errorf("writing plan file: %w", err)
	}
	return nil
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:         ToolNameWritePlan,
			Category:     "plan",
			Description:  "Create or update a shared plan by name. Replaces the entire plan content, so read it first to preserve anything you want to keep. Each write bumps the revision number.",
			Parameters:   tools.MustSchemaFor[WritePlanArgs](),
			OutputSchema: tools.MustSchemaFor[Plan](),
			Handler:      tools.NewHandler(t.writePlan),
			Annotations: tools.ToolAnnotations{
				Title: "Write Plan",
			},
		},
		{
			Name:         ToolNameReadPlan,
			Category:     "plan",
			Description:  "Read a shared plan by name, including its title, content, author, status, and revision number.",
			Parameters:   tools.MustSchemaFor[ReadPlanArgs](),
			OutputSchema: tools.MustSchemaFor[Plan](),
			Handler:      tools.NewHandler(t.readPlan),
			Annotations: tools.ToolAnnotations{
				Title:        "Read Plan",
				ReadOnlyHint: true,
			},
		},
		{
			Name:         ToolNameUpdatePlanFromFile,
			Category:     "plan",
			Description:  "Create or update a shared plan, taking the new content from a file on disk instead of inline. Use it together with export_plan_to_file to edit a large plan without re-sending its whole body. Each write bumps the revision number.",
			Parameters:   tools.MustSchemaFor[UpdatePlanFromFileArgs](),
			OutputSchema: tools.MustSchemaFor[Plan](),
			Handler:      tools.NewHandler(t.updatePlanFromFile),
			Annotations: tools.ToolAnnotations{
				Title: "Update Plan From File",
			},
		},
		{
			Name:         ToolNameExportPlanToFile,
			Category:     "plan",
			Description:  "Write a shared plan's content to a file on disk. The content is written to the file and is NOT returned, so you can materialise a plan cheaply before editing it and committing the result with update_plan_from_file.",
			Parameters:   tools.MustSchemaFor[ExportPlanToFileArgs](),
			OutputSchema: tools.MustSchemaFor[ExportResult](),
			Handler:      tools.NewHandler(t.exportPlanToFile),
			Annotations: tools.ToolAnnotations{
				Title: "Export Plan To File",
			},
		},
		{
			Name:         ToolNameSetPlanStatus,
			Category:     "plan",
			Description:  "Set a shared plan's free-form status (e.g. 'in-progress', 'blocked', 'done') without rewriting its body. The plan must already exist. Each call bumps the revision number.",
			Parameters:   tools.MustSchemaFor[SetPlanStatusArgs](),
			OutputSchema: tools.MustSchemaFor[StatusView](),
			Handler:      tools.NewHandler(t.setPlanStatus),
			Annotations: tools.ToolAnnotations{
				Title: "Set Plan Status",
			},
		},
		{
			Name:         ToolNameGetPlanStatus,
			Category:     "plan",
			Description:  "Read a shared plan's free-form status and current revision without fetching its body.",
			Parameters:   tools.MustSchemaFor[GetPlanStatusArgs](),
			OutputSchema: tools.MustSchemaFor[StatusView](),
			Handler:      tools.NewHandler(t.getPlanStatus),
			Annotations: tools.ToolAnnotations{
				Title:        "Get Plan Status",
				ReadOnlyHint: true,
			},
		},
		{
			Name:         ToolNameListPlans,
			Category:     "plan",
			Description:  "List all shared plans with their name, title, author, status, and revision.",
			OutputSchema: tools.MustSchemaFor[ListResult](),
			Handler:      t.listPlans,
			Annotations: tools.ToolAnnotations{
				Title:        "List Plans",
				ReadOnlyHint: true,
			},
		},
		{
			Name:        ToolNameDeletePlan,
			Category:    "plan",
			Description: "Delete a shared plan by name.",
			Parameters:  tools.MustSchemaFor[DeletePlanArgs](),
			Handler:     tools.NewHandler(t.deletePlan),
			Annotations: tools.ToolAnnotations{
				Title:           "Delete Plan",
				DestructiveHint: new(true),
			},
		},
	}, nil
}

// FilesystemStorage is the default Storage. It persists each plan as a JSON
// file named <name>.json in a directory, with atomic temp+rename writes, plan
// name validation, and unreadable-file warnings on List. Upsert and Delete
// hold an exclusive advisory lock on a persistent sentinel file
// (lockFileName) in the same directory, so their check-and-mutate windows are
// serialized against other processes sharing the directory and a stale
// ExpectedRevision always fails deterministically instead of clobbering a
// concurrent write. A mutex serializes the local check-and-mutate windows
// within the process; mutations take it only after the sentinel lock is held,
// so a writer waiting for a contended sentinel never blocks another local
// mutation's chance to fail fast on the sentinel. Get and List take neither
// the file lock nor the mutex: writes are atomic (temp + rename), so a
// lock-free reader always sees a complete plan — either the previous or the
// new revision, never a partial file — and a mutation blocked on wedged
// storage can never stall readers.
type FilesystemStorage struct {
	mu  sync.Mutex
	dir string
}

var _ Storage = (*FilesystemStorage)(nil)

// NewFilesystemStorage returns a filesystem-backed Storage rooted at dir. The
// directory is created lazily on the first write, not here, so a parent that is
// momentarily unavailable at startup is recovered from automatically.
func NewFilesystemStorage(dir string) *FilesystemStorage {
	return &FilesystemStorage{dir: dir}
}

// String renders the backend for ToolSet.Describe, e.g. "dir=/path/to/plans".
func (s *FilesystemStorage) String() string {
	return "dir=" + s.dir
}

// planPath validates name and returns the absolute path of its plan file. The
// name is rejected (rather than rewritten) when it does not match namePattern,
// which guarantees a one-to-one mapping between names and files and prevents
// path traversal.
func (s *FilesystemStorage) planPath(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, name+".json"), nil
}

// load reads and decodes the plan at path. It distinguishes a missing plan
// (false, nil) from a real failure such as a permission error or corrupt JSON
// (false, err), so callers can report the latter instead of masking it as
// "not found". The open is hang-safe (see OpenContentFile) and the descriptor
// is validated before any read — a .json entry that is a directory, device,
// or FIFO is a foreign file, reported as a typed *CorruptPlanError — so a
// FIFO with no writer can never wedge Get, List, or an Upsert's pre-read.
// Checking the opened descriptor (File.Stat), never the path, means a
// concurrent path swap cannot slip a non-regular file past the check. The
// read is bounded by maxEncodedPlanSize: this package never persists a
// larger file (save validates content and metadata against caps that always
// encode within it), so anything beyond it is a foreign or damaged file and
// is reported as corrupt rather than being slurped into memory whole.
func (s *FilesystemStorage) load(path string) (Plan, bool, error) {
	f, err := OpenContentFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Plan{}, false, nil
	}
	if err != nil {
		return Plan{}, false, fmt.Errorf("reading plan: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return Plan{}, false, fmt.Errorf("reading plan: %w", err)
	}
	if info.IsDir() {
		return Plan{}, false, &CorruptPlanError{File: filepath.Base(path), Err: errors.New("is a directory, not a plan file")}
	}
	if !info.Mode().IsRegular() {
		return Plan{}, false, &CorruptPlanError{File: filepath.Base(path), Err: errors.New("not a regular file (e.g. a device or named pipe)")}
	}

	// Read one byte past the cap so an over-cap file is detected without
	// trusting a stat size that could change under us.
	data, err := io.ReadAll(io.LimitReader(f, maxEncodedPlanSize+1))
	if err != nil {
		return Plan{}, false, fmt.Errorf("reading plan: %w", err)
	}
	if len(data) > maxEncodedPlanSize {
		return Plan{}, false, &CorruptPlanError{File: filepath.Base(path), Err: fmt.Errorf("file exceeds %d bytes", maxEncodedPlanSize)}
	}
	var p Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return Plan{}, false, &CorruptPlanError{File: filepath.Base(path), Err: err}
	}
	return p, true, nil
}

func (s *FilesystemStorage) save(path string, p Plan) error {
	// Validate the content cap before marshaling so an oversized plan is
	// refused with a precise message and without first allocating its
	// encoding. Content of exactly MaxPlanContentSize is accepted. Metadata
	// (title, author, status) is free-form and has no per-field cap: plans
	// stored by earlier builds must keep loading and re-saving whatever
	// labels they carry.
	if len(p.Content) > MaxPlanContentSize {
		return fmt.Errorf("plan %q content is too large to store (%d bytes; max %d)", p.Name, len(p.Content), MaxPlanContentSize)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("creating plans directory: %w", err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling plan: %w", err)
	}
	// Storage limit, not a semantic cap: valid content always fits (the
	// bound absorbs its worst-case escaping), so only pathological aggregate
	// metadata can land here, and a plan is never persisted in a form load
	// would then refuse to decode.
	if len(data) > maxEncodedPlanSize {
		return fmt.Errorf("plan %q is too large to store (%d bytes; max %d)", p.Name, len(data), maxEncodedPlanSize)
	}
	// Atomic write (temp file + rename): readers in other agents or processes
	// see either the old or the new content, never a partial file, and an
	// existing symlink at path is replaced rather than followed.
	if err := atomicfile.Write(path, bytes.NewReader(data), 0o600); err != nil {
		return fmt.Errorf("writing plan: %w", err)
	}
	return nil
}

func (s *FilesystemStorage) Get(ctx context.Context, name string) (Plan, bool, error) {
	path, err := s.planPath(name)
	if err != nil {
		return Plan{}, false, err
	}
	// Observe cancellation before touching the filesystem. A blocked open on
	// truly wedged storage (e.g. a dead NFS mount) cannot be interrupted, but
	// a caller whose deadline already expired gets its context error instead
	// of another read.
	if err := ctx.Err(); err != nil {
		return Plan{}, false, err
	}

	// Deliberately lock-free (no s.mu): writes are atomic (temp + rename), so
	// this always reads a complete plan, and a mutation blocked on wedged
	// storage while holding s.mu can never stall a reader.
	plan, ok, err := s.load(path)
	if ok {
		// The filename is the authoritative key (List and Upsert key off it), so
		// normalise the returned Name to the requested one. A plan file whose
		// stored "name" field drifted from its filename then reads back under the
		// name the caller can actually use to write or delete it.
		plan.Name = name
	}
	return plan, ok, err
}

func (s *FilesystemStorage) Upsert(ctx context.Context, req UpsertRequest) (Plan, error) {
	path, err := s.planPath(req.Name)
	if err != nil {
		return Plan{}, err
	}

	// Lock order: cross-process sentinel first, s.mu second, with s.mu held
	// only around the local load/check/save window. s.mu serializes the
	// check-and-mutate windows of same-process writers where the sentinel
	// lock is a no-op (js/wasm, wasip1, plan9); Get and List are lock-free,
	// so a mutation blocked here never stalls readers. Every mutation takes
	// the locks in this order, so they cannot deadlock.
	release, err := acquireFileLock(ctx, filepath.Join(s.dir, lockFileName))
	if err != nil {
		return Plan{}, err
	}
	defer release()

	s.mu.Lock()
	defer s.mu.Unlock()

	plan, exists, err := s.load(path)
	if err != nil {
		return Plan{}, err
	}
	if req.MustExist && !exists {
		return Plan{}, fmt.Errorf("%w: %q", ErrPlanNotFound, req.Name)
	}
	// Create-only guard: any existing plan conflicts, whatever its revision.
	// Existence is checked directly rather than through the optimistic lock
	// below because revision 0 is not proof of absence — a stored plan file
	// that omits the revision field legitimately reads back as revision 0
	// and must not be overwritten by a create.
	if req.MustNotExist && exists {
		return Plan{}, &VersionConflictError{Name: req.Name, Expected: 0, Current: plan.Revision}
	}
	// Optimistic-lock check: reject the write if another writer bumped the
	// revision since the caller last read it. Checked while both the mutex and
	// the cross-process file lock are held, so the compare-and-set is atomic
	// against writers in this process and in any other process sharing the
	// directory.
	if req.ExpectedRevision != nil && plan.Revision != *req.ExpectedRevision {
		return Plan{}, &VersionConflictError{Name: req.Name, Expected: *req.ExpectedRevision, Current: plan.Revision}
	}

	plan.Name = req.Name
	// Nil fields are preserved across revisions, so an agent updating only the
	// content (or only the status) does not wipe metadata set by a previous
	// writer.
	if req.Content != nil {
		plan.Content = *req.Content
	}
	if req.Title != nil {
		plan.Title = *req.Title
	}
	if req.Author != nil {
		plan.Author = *req.Author
	}
	if req.Status != nil {
		plan.Status = *req.Status
	}
	plan.Revision++
	plan.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	if err := s.save(path, plan); err != nil {
		return Plan{}, err
	}

	return plan, nil
}

func (s *FilesystemStorage) List(ctx context.Context) ([]Summary, []string, error) {
	// Deliberately lock-free (no s.mu), like Get: every plan file is read
	// through the same atomic-write guarantee, so holding the mutex across a
	// whole directory walk would only let a wedged mutation stall listings.
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Summary{}, nil, nil
		}
		return nil, nil, err
	}

	plans := []Summary{}
	var warnings []string
	for _, entry := range entries {
		// The directory is read file by file; observe cancellation between
		// files so an expired deadline stops the walk instead of pointlessly
		// finishing it.
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		// The filename is the authoritative key: read_plan and delete_plan
		// resolve a name to <name>.json, so the listed name must match the
		// filename, not the (possibly drifted) name field inside the JSON.
		name := strings.TrimSuffix(entry.Name(), ".json")
		plan, ok, err := s.load(filepath.Join(s.dir, entry.Name()))
		if err != nil || !ok {
			// Don't abort the whole listing on one bad file, but surface it as
			// a warning so the caller doesn't mistake an unreadable plan for a
			// missing one.
			msg := "unreadable"
			if err != nil {
				msg = err.Error()
			}
			warnings = append(warnings, fmt.Sprintf("skipped %q: %s", name, msg))
			continue
		}
		plans = append(plans, Summary{
			Name:      name,
			Title:     plan.Title,
			Author:    plan.Author,
			Status:    plan.Status,
			Revision:  plan.Revision,
			UpdatedAt: plan.UpdatedAt,
		})
	}

	slices.SortFunc(plans, func(a, b Summary) int {
		return cmp.Compare(a.Name, b.Name)
	})

	return plans, warnings, nil
}

func (s *FilesystemStorage) Delete(ctx context.Context, name string, expectedRevision *int) (bool, error) {
	path, err := s.planPath(name)
	if err != nil {
		return false, err
	}

	// Same lock order as Upsert: sentinel first, s.mu only around the local
	// load/check/remove window, so a delete blocked on the sentinel never
	// stalls readers.
	release, err := acquireFileLock(ctx, filepath.Join(s.dir, lockFileName))
	if err != nil {
		return false, err
	}
	defer release()

	s.mu.Lock()
	defer s.mu.Unlock()

	// With an optimistic-lock guard we must read the current revision first, so
	// the compare-and-delete is atomic under the locks (in-process mutex plus
	// cross-process file lock). This means a guarded delete of a corrupt plan
	// fails (its revision can't be read to compare); recovering from a corrupt
	// plan is done with an unguarded delete, which removes the file directly
	// without pre-loading it.
	if expectedRevision != nil {
		plan, ok, err := s.load(path)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		if plan.Revision != *expectedRevision {
			return false, &VersionConflictError{Name: name, Expected: *expectedRevision, Current: plan.Revision}
		}
	}

	// Remove the file directly and treat a missing file as "not deleted".
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}

	return true, nil
}
