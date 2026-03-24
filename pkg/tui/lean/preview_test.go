package lean

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFormatToolPreviewShellUsesCmdField(t *testing.T) {
	preview := formatToolPreview("shell", `{"cmd":"go test ./...","cwd":"/tmp"}`, ToolRunning, "", false)
	require.Equal(t, "$ go test ./...", preview.Summary)
	require.Contains(t, preview.Details, "cwd: /tmp")
}

func TestFormatToolPreviewBackgroundJobUsesCmdField(t *testing.T) {
	preview := formatToolPreview("run_background_job", `{"cmd":"npm run dev"}`, ToolPending, "", false)
	require.Equal(t, "$ npm run dev", preview.Summary)
}
