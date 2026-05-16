// Package messages defines all TUI message types organized by domain.
//
// Messages are grouped into domain-specific files:
//   - session.go: Session lifecycle (new, exit, save, load, etc.)
//   - theme.go: Theme selection and preview
//   - agent.go: Agent switching and model selection
//   - toggle.go: UI state toggles (YOLO, sidebar)
//   - input.go: Editor input, attachments, and speech
//   - mcp.go: MCP prompt interactions
//
// This organization follows the Elm Architecture principle of grouping
// messages by the domain they affect, making it easier to understand
// which components handle which messages.
package messages
