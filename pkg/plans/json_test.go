package plans

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the host-facing JSON wire contract: field names are stable
// snake_case, absent values are omitted, and the zero UpdatedAt is dropped by
// omitzero (time.Time.IsZero) rather than serialized as "0001-01-01...".
// JSONEq compares full key sets and values, so any accidental tag change
// fails loudly.

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return string(data)
}

func TestPlanJSON_SharedShape(t *testing.T) {
	t.Parallel()
	p := Plan{
		Scope:     ScopeShared,
		Name:      "release",
		Title:     "Release plan",
		Author:    "alice",
		Status:    "draft",
		Content:   "body",
		Version:   new(3),
		UpdatedAt: time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC),
	}

	assert.JSONEq(t,
		`{"scope":"shared","name":"release","title":"Release plan","author":"alice","status":"draft","content":"body","version":3,"updated_at":"2024-05-06T07:08:09Z"}`,
		mustMarshal(t, p))
}

func TestPlanJSON_ZeroValuesOmitted(t *testing.T) {
	t.Parallel()

	// A zero UpdatedAt means "unknown" and must be omitted, not rendered as
	// the zero time; all optional strings and the nil version disappear too.
	assert.JSONEq(t,
		`{"scope":"shared","name":"p"}`,
		mustMarshal(t, Plan{Scope: ScopeShared, Name: "p"}))
}

func TestPlanJSON_RoundTrip(t *testing.T) {
	t.Parallel()
	p := Plan{
		Scope:     ScopeShared,
		Name:      "release",
		Title:     "Release plan",
		Author:    "alice",
		Status:    "draft",
		Content:   "body",
		Version:   new(3),
		UpdatedAt: time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC),
	}

	var got Plan
	require.NoError(t, json.Unmarshal([]byte(mustMarshal(t, p)), &got))
	assert.Equal(t, p, got)
}

func TestExportResultJSON_Shapes(t *testing.T) {
	t.Parallel()

	assert.JSONEq(t,
		`{"scope":"shared","name":"p","path":"/out/plan.md","version":2,"bytes_written":5}`,
		mustMarshal(t, ExportResult{Scope: ScopeShared, Name: "p", Path: "/out/plan.md", Version: new(2), BytesWritten: 5}))

	// A nil version is omitted from the wire shape.
	assert.JSONEq(t,
		`{"scope":"shared","name":"p","path":"/out/plan.md","bytes_written":0}`,
		mustMarshal(t, ExportResult{Scope: ScopeShared, Name: "p", Path: "/out/plan.md"}))
}
