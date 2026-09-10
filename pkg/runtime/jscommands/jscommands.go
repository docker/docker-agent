// Package jscommands wires pkg/js's goja-backed evaluator into the runtime,
// enabling ${...} JavaScript expressions in slash-command instructions:
//
//	jscommands.Register()
//
// It is a separate package (called by loaderdefaults.Opts, the docker-agent CLI and
// embeddedchat/defaults) so embedders that build teams in code and don't use
// JS command expressions never link the JS engine into their binary.
package jscommands

import (
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

// Register installs the goja-backed evaluator as the runtime's command
// evaluator. It is idempotent and safe to call from multiple goroutines.
func Register() {
	runtime.RegisterCommandEvaluator(Factory)
}

// Factory builds the JavaScript evaluator without changing global registration.
// Pass it to runtime.WithCommandEvaluatorFactory for instance-scoped wiring.
func Factory(agentTools []tools.Tool) runtime.CommandEvaluator {
	return js.NewEvaluator(agentTools)
}
