//go:build docker_agent_no_hook_templates

package hooks

func parsePrompt(string) (func(*Input) (string, error), error) {
	return nil, ErrHookTemplatesDisabled
}
