package session

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestSessionAttributesConcurrent(t *testing.T) {
	t.Parallel()

	s := New(WithAttributes(map[string]string{"daw.workspace_path": "/workspace"}))
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			s.SetAttribute("daw.worktree_id", "worktree")
		})
		wg.Go(func() {
			_ = s.AttributesSnapshot()
		})
		wg.Go(func() {
			s.DeleteAttribute("daw.worktree_id")
		})
		wg.Go(func() {
			if _, err := json.Marshal(s); err != nil {
				t.Errorf("Marshal: %v", err)
			}
		})
	}
	wg.Wait()
}

func TestAddMessageUsageRecordConcurrent(t *testing.T) {
	t.Parallel()

	s := New()
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			s.AddMessageUsageRecord("agent", "model", 0.1, &chat.Usage{InputTokens: 10, OutputTokens: 5})
		})
	}
	wg.Wait()
	if got := len(s.MessageUsageHistorySnapshot()); got != 100 {
		t.Errorf("expected 100 records, got %d", got)
	}
}

// TestCompactionInputConcurrent pins the data-race fix for the
// compactor: CompactionInput must read s.Messages under s.mu (via
// snapshotItems) so it stays safe against concurrent AddMessage and
// ApplyCompaction calls. Run with -race; without the lock the slice
// header read aliases the live backing array and the race detector
// flags the AddMessage append.
func TestCompactionInputConcurrent(t *testing.T) {
	t.Parallel()

	s := New()
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			s.AddMessage(&Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "u"}})
		})
		wg.Go(func() {
			_, _, _ = s.CompactionInput()
		})
	}
	// One concurrent ApplyCompaction-shaped write to exercise the same
	// lock from a writer that also bumps the cumulative token counts.
	wg.Go(func() {
		s.ApplyCompaction(0, 0, Item{Summary: "snap"})
	})
	wg.Wait()
}

// TestAddMessageReturnsAppendedIndex pins AddMessage's return-value
// contract: the index of the item it just appended. Hot paths (e.g.
// pkg/runtime/loop.go's UserMessageEvent emission) rely on this instead of
// a separate len(sess.Messages)-1 read, which would race with a concurrent
// AddMessage/ApplyCompaction and could observe a later, larger length.
func TestAddMessageReturnsAppendedIndex(t *testing.T) {
	t.Parallel()

	s := New()
	if got := s.AddMessage(UserMessage("a")); got != 0 {
		t.Errorf("expected index 0, got %d", got)
	}
	if got := s.AddMessage(UserMessage("b")); got != 1 {
		t.Errorf("expected index 1, got %d", got)
	}
}

// TestAddMessageConcurrentReturnsUniqueIndices pins the atomicity of
// AddMessage's returned index under concurrent callers: every call must
// observe the position of its own append (under s.mu), never a stale or
// duplicate one, which is what makes the returned value safe to stamp an
// event's SessionPosition with instead of reading len(sess.Messages)-1
// after the fact.
func TestAddMessageConcurrentReturnsUniqueIndices(t *testing.T) {
	t.Parallel()

	s := New()
	const n = 200
	indices := make(chan int, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			indices <- s.AddMessage(&Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "u"}})
		})
	}
	wg.Wait()
	close(indices)

	seen := make(map[int]bool, n)
	for idx := range indices {
		if seen[idx] {
			t.Fatalf("duplicate index %d returned by concurrent AddMessage calls", idx)
		}
		seen[idx] = true
	}
	if len(seen) != n {
		t.Errorf("expected %d unique indices, got %d", n, len(seen))
	}
}

// TestBranchSessionConcurrent pins the data-race fix for branch/fork:
// branchSessionWithTitle (BranchSession/ForkSession) must read
// parent.Messages under parent.mu (via snapshotItems), mirroring Clone,
// so it stays safe against a concurrent AddMessage on the same live
// session — e.g. the HTTP AddMessage path racing a TUI branch/fork action.
// Run with -race; without the lock the branchAtPosition bounds check and
// the parent.Messages[i] reads alias the live backing array and the race
// detector flags the AddMessage append.
func TestBranchSessionConcurrent(t *testing.T) {
	t.Parallel()

	parent := New(WithUserMessage("seed"))
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			parent.AddMessage(&Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "u"}})
		})
		wg.Go(func() {
			if _, err := BranchSession(parent, 1); err != nil {
				t.Errorf("BranchSession: %v", err)
			}
		})
		wg.Go(func() {
			if _, err := ForkSession(parent, 1); err != nil {
				t.Errorf("ForkSession: %v", err)
			}
		})
	}
	wg.Wait()
}

// TestForkSessionConcurrentWithSubSessionMutation pins the same fix for
// cloneSubSession: forking a session that contains a sub-session (e.g. a
// background agent task) must snapshot the sub-session's own Messages
// under its own mu, since that sub-session can still be actively appended
// to while the top-level session is being forked.
func TestForkSessionConcurrentWithSubSessionMutation(t *testing.T) {
	t.Parallel()

	sub := New(WithUserMessage("sub-seed"))
	parent := New(WithUserMessage("seed"))
	parent.AddSubSession(sub)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			sub.AddMessage(&Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "u"}})
		})
		wg.Go(func() {
			if _, err := ForkSession(parent, 2); err != nil {
				t.Errorf("ForkSession: %v", err)
			}
		})
	}
	wg.Wait()
}

// TestSetTitleAndSetTokensAndCost pins the setter value contracts: SetTitle
// stores the given title (including via the WithTitle constructor option)
// and SetTokensAndCost stores both token counts and the session cost
// together.
func TestSetTitleAndSetTokensAndCost(t *testing.T) {
	t.Parallel()

	s := New(WithTitle("initial"))
	if got := s.TitleSnapshot(); got != "initial" {
		t.Errorf("expected title %q, got %q", "initial", got)
	}

	s.SetTitle("renamed")
	if got := s.TitleSnapshot(); got != "renamed" {
		t.Errorf("expected title %q, got %q", "renamed", got)
	}

	s.SetTokensAndCost(11, 22, 0.5)
	input, output, cost := s.TokensAndCost()
	if input != 11 || output != 22 {
		t.Errorf("expected usage (11, 22), got (%d, %d)", input, output)
	}
	if cost != 0.5 {
		t.Errorf("expected cost 0.5, got %v", cost)
	}
}

// TestSetTitleSetTokensAndCostConcurrent pins the data-race fix for the
// session's scalar metadata: Title, InputTokens/OutputTokens, and Cost used
// to be written directly cross-goroutine (e.g. the server's title updates
// racing the persistence observer's UpdateSession snapshot). SetTitle and
// SetTokensAndCost must take s.mu so writers cannot race each other or the
// locked readers (TitleSnapshot, Usage, TokensAndCost). Run with -race.
//
// Every SetTokensAndCost call writes a related (input, output, cost) triple;
// the post-Wait check asserts the surviving triple is internally consistent,
// pinning that the three fields are assigned atomically rather than torn
// across concurrent calls. The assertion is order-independent, so it cannot
// flake on scheduling.
func TestSetTitleSetTokensAndCostConcurrent(t *testing.T) {
	t.Parallel()

	s := New()
	var wg sync.WaitGroup
	for i := range 100 {
		n := int64(i + 1)
		wg.Go(func() {
			s.SetTitle("concurrent title")
		})
		wg.Go(func() {
			s.SetTokensAndCost(n, 2*n, float64(n)/2)
		})
		wg.Go(func() {
			_, _ = s.Usage()
		})
		wg.Go(func() {
			_ = s.TitleSnapshot()
		})
		wg.Go(func() {
			_, _, _ = s.TokensAndCost()
		})
	}
	wg.Wait()

	input, output, cost := s.TokensAndCost()
	if output != 2*input || cost != float64(input)/2 {
		t.Errorf("torn token/cost triple: input=%d output=%d cost=%v", input, output, cost)
	}
	if got := s.TitleSnapshot(); got != "concurrent title" {
		t.Errorf("expected title %q, got %q", "concurrent title", got)
	}
}

// TestInMemoryStoreGranularUpdatesConcurrent pins the data-race fix for
// InMemorySessionStore.UpdateSessionTokens and UpdateSessionTitle: they used
// to write the live session's Title/InputTokens/OutputTokens/Cost fields
// directly, racing each other and the UpdateSession snapshot (which reads
// those fields under session.mu). Both must route through the session's
// locked setters. Run with -race.
func TestInMemoryStoreGranularUpdatesConcurrent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := NewInMemorySessionStore()
	sess := New()
	if err := store.AddSession(ctx, sess); err != nil {
		t.Fatalf("AddSession: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 100 {
		n := int64(i + 1)
		wg.Go(func() {
			if err := store.UpdateSessionTokens(ctx, sess.ID, n, n, float64(n)); err != nil {
				t.Errorf("UpdateSessionTokens: %v", err)
			}
		})
		wg.Go(func() {
			if err := store.UpdateSessionTitle(ctx, sess.ID, "concurrent title"); err != nil {
				t.Errorf("UpdateSessionTitle: %v", err)
			}
		})
		wg.Go(func() {
			if err := store.UpdateSession(ctx, sess); err != nil {
				t.Errorf("UpdateSession: %v", err)
			}
		})
	}
	wg.Wait()
}

// TestInMemoryStoreGetSessionSummariesConcurrent pins the read-side of the
// same fix: GetSessionSummaries reads each live session's Title and Cost, so
// it must use locked accessors to stay safe against concurrent metadata
// updates on the shared pointer. Run with -race; direct field reads make the
// detector flag the concurrent writes.
func TestInMemoryStoreGetSessionSummariesConcurrent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := NewInMemorySessionStore()
	sess := New()
	if err := store.AddSession(ctx, sess); err != nil {
		t.Fatalf("AddSession: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 100 {
		n := int64(i + 1)
		wg.Go(func() {
			if err := store.UpdateSessionTitle(ctx, sess.ID, "concurrent title"); err != nil {
				t.Errorf("UpdateSessionTitle: %v", err)
			}
		})
		wg.Go(func() {
			sess.SetTitle("direct title")
		})
		wg.Go(func() {
			if err := store.UpdateSessionTokens(ctx, sess.ID, n, n, float64(n)); err != nil {
				t.Errorf("UpdateSessionTokens: %v", err)
			}
		})
		wg.Go(func() {
			if _, err := store.GetSessionSummaries(ctx); err != nil {
				t.Errorf("GetSessionSummaries: %v", err)
			}
		})
	}
	wg.Wait()

	summaries, err := store.GetSessionSummaries(ctx)
	if err != nil {
		t.Fatalf("GetSessionSummaries: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(summaries))
	}
	if got := summaries[0].Title; got != "concurrent title" && got != "direct title" {
		t.Errorf("unexpected title %q", got)
	}
	_, _, wantCost := sess.TokensAndCost()
	if got := summaries[0].Cost; got != wantCost {
		t.Errorf("expected cost %v, got %v", wantCost, got)
	}
}
