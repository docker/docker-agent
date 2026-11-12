// Package defaults supplies the full builtin tool renderer bundle.
package defaults

import (
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
	handofftool "github.com/docker/docker-agent/pkg/tools/builtin/handoff/types"
	plan "github.com/docker/docker-agent/pkg/tools/builtin/plan/types"
	shelltool "github.com/docker/docker-agent/pkg/tools/builtin/shell/types"
	todo "github.com/docker/docker-agent/pkg/tools/builtin/todo/types"
	transfertasktool "github.com/docker/docker-agent/pkg/tools/builtin/transfertask/types"
	userpromptool "github.com/docker/docker-agent/pkg/tools/builtin/userprompt/types"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/components/tool/codemode"
	"github.com/docker/docker-agent/pkg/tui/components/tool/directorytree"
	"github.com/docker/docker-agent/pkg/tui/components/tool/editfile"
	"github.com/docker/docker-agent/pkg/tui/components/tool/handoff"
	"github.com/docker/docker-agent/pkg/tui/components/tool/listdirectory"
	"github.com/docker/docker-agent/pkg/tui/components/tool/plantool"
	"github.com/docker/docker-agent/pkg/tui/components/tool/readfile"
	"github.com/docker/docker-agent/pkg/tui/components/tool/readmultiplefiles"
	"github.com/docker/docker-agent/pkg/tui/components/tool/searchfilescontent"
	"github.com/docker/docker-agent/pkg/tui/components/tool/shell"
	"github.com/docker/docker-agent/pkg/tui/components/tool/todotool"
	"github.com/docker/docker-agent/pkg/tui/components/tool/transfertask"
	"github.com/docker/docker-agent/pkg/tui/components/tool/userprompt"
	"github.com/docker/docker-agent/pkg/tui/components/tool/writefile"
)

// NewRegistry creates an independent registry with all builtin renderers.
func NewRegistry() *tool.Registry {
	builders := map[string]tool.Builder{
		"run_tools_with_javascript":           codemode.New,
		transfertasktool.ToolNameTransferTask: transfertask.New,
		handofftool.ToolNameHandoff:           handoff.New,
		filesystem.ToolNameEditFile:           editfile.New,
		filesystem.ToolNameWriteFile:          writefile.New,
		filesystem.ToolNameReadFile:           readfile.New,
		filesystem.ToolNameReadMultipleFiles:  readmultiplefiles.New,
		filesystem.ToolNameListDirectory:      listdirectory.New,
		filesystem.ToolNameDirectoryTree:      directorytree.New,
		filesystem.ToolNameSearchFilesContent: searchfilescontent.New,
		shelltool.ToolNameShell:               shell.New,
		userpromptool.ToolNameUserPrompt:      userprompt.New,
		todo.ToolNameCreateTodo:               todotool.New,
		todo.ToolNameCreateTodos:              todotool.New,
		todo.ToolNameUpdateTodos:              todotool.New,
		todo.ToolNameListTodos:                todotool.New,
		// Single-plan write/status tools surface the plan's status/title in a
		// compact header. read_plan, list_plans and delete_plan intentionally keep
		// the default renderer: read_plan's job is to show the full plan body (the
		// default renderer prints it, with the status still in the JSON), list_plans
		// returns many plans, and delete_plan has no status to show.
		plan.ToolNameWritePlan:          plantool.New,
		plan.ToolNameSetPlanStatus:      plantool.New,
		plan.ToolNameGetPlanStatus:      plantool.New,
		plan.ToolNameUpdatePlanFromFile: plantool.New,
		plan.ToolNameExportPlanToFile:   plantool.New,
	}

	r := tool.NewRegistry()
	for key, b := range builders {
		r.RegisterBuiltin(key, b)
	}
	r.RegisterCacheInvalidator(editfile.InvalidateCaches)
	return r
}
