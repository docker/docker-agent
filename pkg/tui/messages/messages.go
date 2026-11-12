// Package messages defines all TUI message types organized by domain.
//
// Messages are grouped into domain-specific files:
//   - session.go: Session lifecycle (new, exit, save, load, etc.)
//   - theme.go: Theme selection and preview
//   - agent.go: Agent switching and model selection
//   - toggle.go: UI state toggles (YOLO, sidebar)
//   - input.go: Editor input, attachments, and speech
//   - mcp.go: MCP prompt interactions
//   - plans.go: Plan management (/plans browser)
//
// This organization follows the Elm Architecture principle of grouping
// messages by the domain they affect, making it easier to understand
// which components handle which messages.
package messages

// ToggleDiffViewMsg invalidates views after changing the diff layout.
type ToggleDiffViewMsg struct{}

// ImageRenderingChangedMsg invalidates cached image rows after capability or preference changes.
type ImageRenderingChangedMsg struct{}
