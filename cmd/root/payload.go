package root

import (
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
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
		SafetyPolicy:      session.SafetyPolicy(f.safety),
		HideToolResults:   f.hideToolResults,
		SessionDB:         sessionDBPath(f.sessionDB),
		ResumeSessionID:   f.sessionID,
		SnapshotsEnabled:  f.snapshotsEnabled,
		GlobalPermissions: f.globalPermissions,
		WorkingDir:        workingDir,
	}
}
