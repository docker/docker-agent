package messages

// Agent messages control agent switching, commands, and model selection.
type (
	// SwitchAgentMsg switches to a different agent.
	SwitchAgentMsg struct{ AgentName string }

	// AgentCommandMsg sends a command to the agent.
	AgentCommandMsg struct{ Command string }

	// OpenModelPickerMsg opens the model picker dialog.
	OpenModelPickerMsg struct{}

	// ChangeModelMsg changes the model for the current agent.
	ChangeModelMsg struct{ ModelRef string }
)
