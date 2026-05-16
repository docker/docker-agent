package teamloader

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func mcpCmd(name, command string) latest.Toolset {
	return latest.Toolset{Type: "mcp", Name: name, Command: command}
}

func TestDiffToolsets_NoChanges(t *testing.T) {
	t.Parallel()
	a := mcpCmd("a", "x")
	b := mcpCmd("b", "y")
	diff := DiffToolsets([]latest.Toolset{a, b}, []latest.Toolset{a, b})
	assert.Empty(t, diff.Added)
	assert.Empty(t, diff.Removed)
	assert.Empty(t, diff.Changed)
	assert.Len(t, diff.Unchanged, 2)
	assert.False(t, diff.HasChanges())
}

func TestDiffToolsets_Added(t *testing.T) {
	t.Parallel()
	a := mcpCmd("a", "x")
	b := mcpCmd("b", "y")
	diff := DiffToolsets([]latest.Toolset{a}, []latest.Toolset{a, b})
	assert.Len(t, diff.Added, 1)
	assert.Equal(t, "b", diff.Added[0].Name)
	assert.Empty(t, diff.Removed)
	assert.Empty(t, diff.Changed)
	assert.Len(t, diff.Unchanged, 1)
	assert.True(t, diff.HasChanges())
}

func TestDiffToolsets_Removed(t *testing.T) {
	t.Parallel()
	a := mcpCmd("a", "x")
	b := mcpCmd("b", "y")
	diff := DiffToolsets([]latest.Toolset{a, b}, []latest.Toolset{a})
	assert.Empty(t, diff.Added)
	assert.Len(t, diff.Removed, 1)
	assert.Equal(t, "b", diff.Removed[0].Name)
	assert.Empty(t, diff.Changed)
	assert.Len(t, diff.Unchanged, 1)
	assert.True(t, diff.HasChanges())
}

func TestDiffToolsets_Changed(t *testing.T) {
	t.Parallel()
	a1 := mcpCmd("a", "x")
	a2 := mcpCmd("a", "y") // same identity (mcp/a), different command
	diff := DiffToolsets([]latest.Toolset{a1}, []latest.Toolset{a2})
	assert.Empty(t, diff.Added)
	assert.Empty(t, diff.Removed)
	assert.Len(t, diff.Changed, 1)
	assert.Equal(t, "x", diff.Changed[0].Old.Command)
	assert.Equal(t, "y", diff.Changed[0].New.Command)
	assert.True(t, diff.HasChanges())
}

func TestDiffToolsets_PreservesOrder(t *testing.T) {
	t.Parallel()
	a := mcpCmd("a", "x")
	b := mcpCmd("b", "y")
	c := mcpCmd("c", "z")
	d := mcpCmd("d", "w")

	oldList := []latest.Toolset{a, b, c}
	newList := []latest.Toolset{c, d, a} // c, d, a — b removed, d added, c & a unchanged
	diff := DiffToolsets(oldList, newList)

	assert.Len(t, diff.Added, 1)
	assert.Equal(t, "d", diff.Added[0].Name)
	assert.Len(t, diff.Removed, 1)
	assert.Equal(t, "b", diff.Removed[0].Name)
	// Unchanged appears in new-slice order: c then a.
	assert.Equal(t, []string{"c", "a"}, []string{diff.Unchanged[0].Name, diff.Unchanged[1].Name})
}

func TestToolsetIdentity_NamedFirst(t *testing.T) {
	t.Parallel()
	withName := latest.Toolset{Type: "mcp", Name: "n", Command: "c"}
	withoutName := latest.Toolset{Type: "mcp", Command: "c"}
	assert.Equal(t, "mcp/n", ToolsetIdentity(withName))
	assert.Equal(t, "mcp/cmd/c", ToolsetIdentity(withoutName))
}

func TestToolsetIdentity_RefCommandRemoteFallback(t *testing.T) {
	t.Parallel()
	ref := latest.Toolset{Type: "mcp", Ref: "docker:foo"}
	cmd := latest.Toolset{Type: "mcp", Command: "bar"}
	rem := latest.Toolset{Type: "mcp", Remote: latest.Remote{URL: "https://x"}}
	lsp := latest.Toolset{Type: "lsp", Command: "gopls"}
	assert.Equal(t, "mcp/ref/docker:foo", ToolsetIdentity(ref))
	assert.Equal(t, "mcp/cmd/bar", ToolsetIdentity(cmd))
	assert.Equal(t, "mcp/remote/https://x", ToolsetIdentity(rem))
	assert.Equal(t, "lsp/cmd/gopls", ToolsetIdentity(lsp))
}

func TestToolsetSignature_StableAcrossEqualValues(t *testing.T) {
	t.Parallel()
	a := mcpCmd("x", "y")
	b := mcpCmd("x", "y")
	assert.Equal(t, ToolsetSignature(a), ToolsetSignature(b))
}

func TestToolsetSignature_DifferentForDifferentValues(t *testing.T) {
	t.Parallel()
	a := mcpCmd("x", "one")
	b := mcpCmd("x", "two")
	assert.NotEqual(t, ToolsetSignature(a), ToolsetSignature(b))
}
