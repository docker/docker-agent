package tools

import (
	"fmt"
	"strings"
)

// ToolsetMetadata exposes optional details for toolset identification.
// Implemented by toolsets that can provide additional context for warnings.
type ToolsetMetadata interface {
	ToolsetID() string
}

// ToolsetIdentifier returns a human-readable identifier for a toolset.
// It falls back to the toolset type when no metadata is available.
func ToolsetIdentifier(ts ToolSet) string {
	if meta, ok := As[ToolsetMetadata](ts); ok {
		label := strings.TrimSpace(meta.ToolsetID())
		if label != "" {
			return label
		}
	}

	return fmt.Sprintf("%T", ts)
}
