package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithAttributesClonesInput(t *testing.T) {
	t.Parallel()
	input := map[string]string{
		"daw.workspace_path": "/workspace",
		"":                   "ignored",
	}

	sess := New(WithAttributes(input))
	input["daw.workspace_path"] = "/mutated"
	input["daw.worktree_id"] = "new"

	assert.Equal(t, map[string]string{"daw.workspace_path": "/workspace"}, sess.AttributesSnapshot())
}

func TestAttributesSnapshotClonesOutput(t *testing.T) {
	t.Parallel()
	sess := New(WithAttributes(map[string]string{"daw.worktree_id": "one"}))

	snapshot := sess.AttributesSnapshot()
	snapshot["daw.worktree_id"] = "two"
	snapshot["daw.worktree_path"] = "/other"

	assert.Equal(t, map[string]string{"daw.worktree_id": "one"}, sess.AttributesSnapshot())
}

func TestSetAndDeleteAttribute(t *testing.T) {
	t.Parallel()
	sess := New()

	sess.SetAttribute("daw.execution_type", "worktree")
	sess.SetAttribute("daw.worktree_id", "one")
	sess.SetAttribute("", "ignored")
	assert.Equal(t, map[string]string{
		"daw.execution_type": "worktree",
		"daw.worktree_id":    "one",
	}, sess.AttributesSnapshot())

	sess.SetAttribute("daw.worktree_id", "two")
	sess.DeleteAttribute("daw.execution_type")
	sess.DeleteAttribute("")
	assert.Equal(t, map[string]string{"daw.worktree_id": "two"}, sess.AttributesSnapshot())
}

func TestSessionAttributesJSONRoundTrip(t *testing.T) {
	t.Parallel()
	original := New(WithAttributes(map[string]string{
		"daw.workspace_path": "/workspace",
		"daw.worktree_id":    "wt-1",
	}))

	data, err := json.Marshal(original)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"attributes"`)

	var decoded Session
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, original.AttributesSnapshot(), decoded.AttributesSnapshot())

	decoded.SetAttribute("daw.worktree_id", "wt-2")
	assert.Equal(t, "wt-1", original.AttributesSnapshot()["daw.worktree_id"])
}

func TestCloneAndBranchCopyAttributesIndependently(t *testing.T) {
	t.Parallel()
	parent := New(
		WithAttributes(map[string]string{"daw.worktree_id": "wt-1"}),
		WithUserMessage("hello"),
	)

	clone := parent.Clone()
	branched, err := BranchSession(parent, 1)
	require.NoError(t, err)

	assert.Equal(t, parent.AttributesSnapshot(), clone.AttributesSnapshot())
	assert.Equal(t, parent.AttributesSnapshot(), branched.AttributesSnapshot())

	clone.SetAttribute("daw.worktree_id", "clone")
	branched.SetAttribute("daw.worktree_id", "branch")
	assert.Equal(t, "wt-1", parent.AttributesSnapshot()["daw.worktree_id"])
	assert.Equal(t, "clone", clone.AttributesSnapshot()["daw.worktree_id"])
	assert.Equal(t, "branch", branched.AttributesSnapshot()["daw.worktree_id"])
}
