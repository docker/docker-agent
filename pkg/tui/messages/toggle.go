package messages

// UI toggle messages control various UI state toggles.
type (
	// ToggleYoloMsg toggles YOLO mode (auto-approve tools).
	ToggleYoloMsg struct{}

	// TogglePauseMsg toggles whether the runtime loop is paused at
	// iteration boundaries. The pause takes effect as soon as the
	// in-flight LLM request and its tool calls complete.
	TogglePauseMsg struct{}

	// ToggleHideToolResultsMsg toggles hiding of tool results.
	ToggleHideToolResultsMsg struct{}

	// ToggleSidebarMsg toggles sidebar visibility.
	// The top-level model also handles this to persist the collapsed state.
	ToggleSidebarMsg struct{}

	// SessionToggleChangedMsg is sent after any session toggle (YOLO, split diff, etc.)
	// changes so that components like the sidebar can invalidate their caches.
	SessionToggleChangedMsg struct{}

	// ShowCostDialogMsg shows the cost/usage dialog.
	ShowCostDialogMsg struct{}

	// ShowPermissionsDialogMsg shows the permissions dialog.
	ShowPermissionsDialogMsg struct{}

	// ShowToolsDialogMsg shows the tools dialog. The dialog renders both
	// the live toolset lifecycle (state, restart count, last error) and
	// the tool catalogue grouped by category.
	ShowToolsDialogMsg struct{}

	// RestartToolsetMsg asks the runtime to restart the named toolset by
	// triggering its supervisor's RestartAndWait.
	RestartToolsetMsg struct{ Name string }
)
