package remote

import "github.com/docker/docker-agent/pkg/runtime/inspection"

func nativeExecutionProfile() *inspection.ExecutionCapabilities {
	return &inspection.ExecutionCapabilities{
		Mode: "native", ToolExecution: "docker-agent", ToolApproval: "docker-agent-policy",
		BudgetSupported: true, CompactionSupported: true, MidTurnSteeringSupported: true, PromptHookContextSupported: true,
	}
}

func harnessExecutionProfile() *inspection.ExecutionCapabilities {
	return &inspection.ExecutionCapabilities{Mode: "harness", ToolExecution: "harness-reported", ToolApproval: "external"}
}
