package root

import (
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
)

// loadTeamRequest builds a runtime.LoadTeamRequest from the current flags.
func (f *runExecFlags) loadTeamRequest(agentSource config.Source) runtime.LoadTeamRequest {
	return runtime.LoadTeamRequest{
		Source:         agentSource,
		ModelOverrides: f.modelOverrides,
		PromptFiles:    f.promptFiles,
		RunConfig:      &f.runConfig,
	}
}

// createSessionRequest builds a runtime.CreateSessionRequest from the
// current flags and the supplied working directory.
func (f *runExecFlags) createSessionRequest(workingDir string) runtime.CreateSessionRequest {
	return runtime.CreateSessionRequest{
		AgentName:         f.agentName,
		ToolsApproved:     f.autoApprove,
		HideToolResults:   f.hideToolResults,
		SessionDB:         f.sessionDB,
		ResumeSessionID:   f.sessionID,
		SnapshotsEnabled:  f.snapshotsEnabled,
		GlobalPermissions: f.globalPermissions,
		WorkingDir:        workingDir,
	}
}
