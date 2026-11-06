package runtime

import (
	"github.com/docker/docker-agent/pkg/runtime/inspection"
)

// AgentExecutionCapabilities inspects the named agent without starting tools or drivers.
// Unknown agents return nil; callers must not interpret absence as native support.
func (r *LocalRuntime) AgentExecutionCapabilities(name string) *inspection.ExecutionCapabilities {
	if r == nil || r.team == nil {
		return nil
	}
	a, err := r.team.Agent(name)
	if err != nil {
		return nil
	}
	return inspection.Capabilities(a)
}
