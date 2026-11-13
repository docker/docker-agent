package toolexec

import "github.com/docker/docker-agent/pkg/permissions"

func commandGrantCoversCall(toolName string, allowPatterns []string, args map[string]any) bool {
	return permissions.CommandAllowCoversCall(toolName, allowPatterns, args)
}
