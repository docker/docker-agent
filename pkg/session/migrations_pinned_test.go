package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

// TestMigrationCatalogIsContentPinned is the append-only enforcement
// reviewers asked for on PR #2646: "while it looks ok in this context,
// it's always dangerous to update an existing migration. A new
// migration would be cheap." Once a migration has shipped, its
// (ID, Name, Description, UpSQL, DownSQL) tuple is part of the on-disk
// schema contract — editing any of those bytes after the fact means an
// already-deployed database may run the new SQL even though the row in
// schema_migrations still records the old name, leaving the schema in
// a state no version of the code has ever produced.
//
// The mechanism is a content hash: every migration's textual fields
// are SHA-256'd into the digest below. Any edit to those fields — or
// any insertion ahead of an existing entry — changes the digest, and
// the test fails with a clear "did you mean to add migration N+1?"
// message. To grow the catalogue:
//
//  1. Append the new migration to getAllMigrations() with the next ID;
//  2. Run the test, copy the actual digest into wantDigest below.
//
// Step 2 is intentionally manual so that the audit trail lives in the
// commit history: every catalogue change is one diff that touches both
// the migrations slice and this constant.
//
// Order matters. The hash is computed over the catalogue in declared
// order; reordering existing entries — even without changing their
// content — flips the digest, which is what we want.
func TestMigrationCatalogIsContentPinned(t *testing.T) {
	t.Parallel()

	got := digestMigrationCatalog(getAllMigrations())

	const wantDigest = "0c6d5df46b970104cf49988ee3931e33643d5db85c68dd41b74b639d0094cec9"
	if got != wantDigest {
		t.Fatalf(`migration catalogue content has changed.

If you added a new migration:
  - confirm its ID is exactly the previous max + 1, and
  - update wantDigest in this test to:
      const wantDigest = %q

If you EDITED an existing migration entry, stop. Migrations are
append-only once shipped (see PR #2646). Revert your edit and add a
new migration instead.

Diff:
  got:  %s
  want: %s
`, got, got, wantDigest)
	}
}

// digestMigrationCatalog returns a deterministic SHA-256 of the
// migration list's textual content, framed so that field-order changes
// inside individual entries also alter the result. UpFunc is
// intentionally excluded — function-pointer identity is not stable
// across builds — so data migrations defined in Go fall outside the
// append-only enforcement and must rely on change-review as the
// safety net.
//
// Migrations that carry their logic in UpFunc (empty UpSQL/DownSQL)
// and are therefore NOT covered by this digest:
//
//   - 015_migrate_messages_to_session_items (migrateMessagesToSessionItems)
//
// Reviewers must scrutinise changes to those functions extra
// carefully, and any future Go-based migration added to the catalogue
// should be appended to the list above.
func digestMigrationCatalog(ms []Migration) string {
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "id=%d\nname=%q\ndesc=%q\nup=%q\ndown=%q\n--\n",
			m.ID, m.Name, m.Description, m.UpSQL, m.DownSQL)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// TestMigrationIDsAreSequential is the construction-side companion to
// TestMigrationCatalogIsContentPinned: even before the digest can fail,
// a missing or out-of-order ID is itself a sign someone reused or
// inserted into the middle of the list. We assert IDs are 1..N with
// no gaps and that each Name starts with its zero-padded ID — the
// convention the catalogue already uses for every existing entry.
func TestMigrationIDsAreSequential(t *testing.T) {
	t.Parallel()

	ms := getAllMigrations()
	for i, m := range ms {
		wantID := i + 1
		if m.ID != wantID {
			t.Errorf("migration[%d]: ID = %d, want %d (catalogue must be 1..N with no gaps)", i, m.ID, wantID)
		}
		// Names look like "001_add_tools_approved_column" — three
		// digits, underscore, then a slug. We only check the prefix.
		// The prefix is derived from wantID (not m.ID) so that this
		// check stays an independent guard: if a middle migration is
		// deleted, m.ID would still match its own "NNN_" prefix and
		// silently mask the gap that the ID check above flagged.
		wantPrefix := fmt.Sprintf("%03d_", wantID)
		if !strings.HasPrefix(m.Name, wantPrefix) {
			t.Errorf("migration[%d]: Name = %q, want prefix %q", i, m.Name, wantPrefix)
		}
	}
}
