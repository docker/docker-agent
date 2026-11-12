package plan

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"
)

// MemoryStorage is a process-local, per-instance Storage that keeps plans in
// a map and never touches the filesystem. It honours the same contract as
// FilesystemStorage — name validation, the content and encoded-size caps,
// the revision bump, and the existence and optimistic-lock guards checked
// atomically with the write — under a single mutex, so agents sharing one
// instance serialize on it. Plans are stored and returned by value: strings
// are immutable and Plan holds no references, so a caller can never mutate
// the store through a returned Plan or through a request it later changes.
//
// Nothing persists beyond the instance. It suits embedders without a
// writable filesystem (e.g. js/wasm) and hosts that want isolated plans per
// session; inject it with WithStorage.
type MemoryStorage struct {
	mu    sync.Mutex
	plans map[string]Plan
}

var (
	_ Storage      = (*MemoryStorage)(nil)
	_ fmt.Stringer = (*MemoryStorage)(nil)
)

// NewMemoryStorage returns an empty in-memory Storage. Each call is an
// independent store.
func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{plans: map[string]Plan{}}
}

// String renders the backend for ToolSet.Describe, e.g. "plan(memory)".
func (s *MemoryStorage) String() string {
	return "memory"
}

func (s *MemoryStorage) Get(ctx context.Context, name string) (Plan, bool, error) {
	if err := ValidateName(name); err != nil {
		return Plan{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return Plan{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[name]
	return p, ok, nil
}

func (s *MemoryStorage) Upsert(ctx context.Context, req UpsertRequest) (Plan, error) {
	if err := ValidateName(req.Name); err != nil {
		return Plan{}, err
	}
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	plan, exists := s.plans[req.Name]
	if req.MustExist && !exists {
		return Plan{}, fmt.Errorf("%w: %q", ErrPlanNotFound, req.Name)
	}
	// Existence-driven, not revision-driven, for the same reason as the
	// filesystem backend: revision 0 is not proof of absence.
	if req.MustNotExist && exists {
		return Plan{}, &VersionConflictError{Name: req.Name, Expected: 0, Current: plan.Revision}
	}
	if req.ExpectedRevision != nil && plan.Revision != *req.ExpectedRevision {
		return Plan{}, &VersionConflictError{Name: req.Name, Expected: *req.ExpectedRevision, Current: plan.Revision}
	}

	plan.Name = req.Name
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

	// Same checks, order and messages as FilesystemStorage.save, measured on
	// the representation it would persist, so both backends accept and refuse
	// exactly the same plans.
	if len(plan.Content) > MaxPlanContentSize {
		return Plan{}, fmt.Errorf("plan %q content is too large to store (%d bytes; max %d)", plan.Name, len(plan.Content), MaxPlanContentSize)
	}
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return Plan{}, fmt.Errorf("marshaling plan: %w", err)
	}
	if len(data) > maxEncodedPlanSize {
		return Plan{}, fmt.Errorf("plan %q is too large to store (%d bytes; max %d)", plan.Name, len(data), maxEncodedPlanSize)
	}

	if s.plans == nil {
		s.plans = map[string]Plan{}
	}
	s.plans[req.Name] = plan
	return plan, nil
}

func (s *MemoryStorage) List(ctx context.Context) ([]Summary, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	plans := make([]Summary, 0, len(s.plans))
	for name, p := range s.plans {
		plans = append(plans, Summary{
			Name:      name,
			Title:     p.Title,
			Author:    p.Author,
			Status:    p.Status,
			Revision:  p.Revision,
			UpdatedAt: p.UpdatedAt,
		})
	}
	slices.SortFunc(plans, func(a, b Summary) int {
		return cmp.Compare(a.Name, b.Name)
	})
	return plans, nil, nil
}

func (s *MemoryStorage) Delete(ctx context.Context, name string, expectedRevision *int) (bool, error) {
	if err := ValidateName(name); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	plan, ok := s.plans[name]
	if !ok {
		return false, nil
	}
	if expectedRevision != nil && plan.Revision != *expectedRevision {
		return false, &VersionConflictError{Name: name, Expected: *expectedRevision, Current: plan.Revision}
	}
	delete(s.plans, name)
	return true, nil
}
