package teamloader

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// ToolsetDiff is the result of comparing two slices of latest.Toolset
// (typically the currently-running set and a freshly-loaded set from
// disk). It is a building block for hot-reload: the runtime can stop
// Removed toolsets, start Added ones, and stop+start Changed ones, while
// leaving Unchanged toolsets running.
//
// Identity is established by ToolsetIdentity (name+type for named
// toolsets, falling back to type+command/url for the rare unnamed case).
// "Changed" is defined as identity-equal but Signature-different.
type ToolsetDiff struct {
	Added     []latest.Toolset
	Removed   []latest.Toolset
	Changed   []ToolsetChange
	Unchanged []latest.Toolset
}

// ToolsetChange records a Toolset whose identity is unchanged but whose
// configuration signature differs.
type ToolsetChange struct {
	Old latest.Toolset
	New latest.Toolset
}

// ToolsetIdentity returns a stable string that identifies a toolset
// across reloads. Named toolsets (the common case for MCP/LSP) are
// keyed by Type+Name. Toolsets without a name fall back to a key
// derived from their type and connection target.
//
// The function is deterministic and total: every Toolset has a unique
// non-empty identity even when several share the same config (the
// fallback includes the JSON signature to break ties).
func ToolsetIdentity(t latest.Toolset) string {
	if t.Name != "" {
		return t.Type + "/" + t.Name
	}
	switch t.Type {
	case "mcp":
		switch {
		case t.Ref != "":
			return "mcp/ref/" + t.Ref
		case t.Command != "":
			return "mcp/cmd/" + t.Command
		case t.Remote.URL != "":
			return "mcp/remote/" + t.Remote.URL
		}
	case "lsp":
		return "lsp/cmd/" + t.Command
	case "a2a":
		return "a2a/" + t.URL
	}
	// Last-resort: type plus signature so two unnamed entries don't collide.
	return t.Type + "/" + ToolsetSignature(t)
}

// ToolsetSignature returns a hex-encoded SHA-256 of the canonical JSON
// representation of t. Two toolsets with byte-identical signatures are
// considered configuration-identical for hot-reload purposes.
//
// Any field present in latest.Toolset participates in the signature, so
// adding new fields automatically participates in change detection
// without further wiring.
func ToolsetSignature(t latest.Toolset) string {
	// json.Marshal sorts struct fields by their declaration order, which
	// is stable; map keys are sorted; slices keep their order. That is
	// good enough for change detection (we don't need cryptographic
	// determinism, only consistency within the process).
	b, err := json.Marshal(t)
	if err != nil {
		// Should not happen for the public Toolset type.
		return fmt.Sprintf("err:%v", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DiffToolsets compares two slices of toolset configs and returns a
// classification of which toolsets were added, removed, changed, or are
// unchanged.
//
// The result preserves the order of the new slice for Added/Changed/
// Unchanged, and the order of the old slice for Removed; this is a
// helpful invariant for status messages.
func DiffToolsets(oldList, newList []latest.Toolset) ToolsetDiff {
	oldByID := make(map[string]latest.Toolset, len(oldList))
	oldOrder := make([]string, 0, len(oldList))
	for _, t := range oldList {
		id := ToolsetIdentity(t)
		// If two old entries share an ID (extremely unlikely, but possible
		// for fully-unnamed toolsets), the second wins for diff purposes;
		// the first will appear as Removed because it has no match in new.
		if _, exists := oldByID[id]; !exists {
			oldOrder = append(oldOrder, id)
		}
		oldByID[id] = t
	}

	seen := make(map[string]bool, len(newList))
	var diff ToolsetDiff
	for _, t := range newList {
		id := ToolsetIdentity(t)
		seen[id] = true
		previous, ok := oldByID[id]
		if !ok {
			diff.Added = append(diff.Added, t)
			continue
		}
		if ToolsetSignature(previous) == ToolsetSignature(t) {
			diff.Unchanged = append(diff.Unchanged, t)
			continue
		}
		diff.Changed = append(diff.Changed, ToolsetChange{Old: previous, New: t})
	}
	for _, id := range oldOrder {
		if !seen[id] {
			diff.Removed = append(diff.Removed, oldByID[id])
		}
	}
	return diff
}

// HasChanges reports whether the diff requires any runtime action. It is
// useful as a fast-path for the hot-reload trigger: if HasChanges is
// false, the runtime can skip the diff entirely.
func (d ToolsetDiff) HasChanges() bool {
	return len(d.Added) > 0 || len(d.Removed) > 0 || len(d.Changed) > 0
}
