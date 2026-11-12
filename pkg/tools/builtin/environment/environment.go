// Package environment exposes a single read-only tool the model can call
// to learn which OS and shell will run its shell-tool commands. The tool
// takes no arguments and returns a fixed shape — reads only runtime.GOOS
// and the already-detected shell binary — so it earns the ReadOnlyHint
// auto-approval used by other pure-info tools (git status, filesystem
// reads, memory reads, …).
package environment

import (
	"context"
	"encoding/json"
	"runtime"

	"github.com/docker/docker-agent/pkg/shellpath"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameGetEnvironmentInfo = "get_environment_info"
	category                   = "environment"
)

// CreateToolSet is the registry entry point.
func CreateToolSet() (tools.ToolSet, error) {
	return New(), nil
}

type ToolSet struct{}

// Args intentionally has no fields: the tool must not accept model- or
// user-steered inputs, so the safety surface is what the code decides to
// read, not what the caller asks for.
type Args struct{}

// Info is the tool's fixed output shape. Deliberately narrow: OS and
// shell only, no working directory, no full paths, no username. Anything
// user-controlled would land in the conversation transcript.
type Info struct {
	OS    string `json:"os"`
	Shell string `json:"shell"`
}

func New() *ToolSet {
	return &ToolSet{}
}

func (t *ToolSet) get(_ context.Context, _ Args) (*tools.ToolCallResult, error) {
	shellPath, _ := shellpath.DetectShell()
	info := Info{
		OS:    displayOS(),
		Shell: shellpath.ShellBaseName(shellPath),
	}
	payload, err := json.Marshal(info)
	if err != nil {
		return tools.ResultError("Error marshaling environment info: " + err.Error()), nil
	}
	return tools.ResultSuccess(string(payload)), nil
}

func (t *ToolSet) Instructions() string {
	return `## Environment Info

Call get_environment_info when you're unsure which shell syntax to use.
Returns the OS and the resolved shell (e.g. {"os":"Windows","shell":"powershell"}),
nothing else. Cheap: no arguments, no side effects, auto-approved.`
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:                    ToolNameGetEnvironmentInfo,
			Category:                category,
			Description:             `Returns the operating system and resolved shell that will run shell-tool commands (e.g. {"os":"Windows","shell":"powershell"}). No arguments, no side effects. Call this when unsure which shell syntax to use.`,
			Parameters:              tools.MustSchemaFor[Args](),
			OutputSchema:            tools.MustSchemaFor[Info](),
			Handler:                 tools.NewHandler(t.get),
			Annotations:             tools.ToolAnnotations{ReadOnlyHint: true, Title: "Environment Info"},
			AddDescriptionParameter: true,
		},
	}, nil
}

// displayOS mirrors the labels used by the shell tool description so the
// value the model sees here matches what it saw in the shell tool schema.
func displayOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	default:
		return runtime.GOOS
	}
}
