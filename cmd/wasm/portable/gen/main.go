// Command gen writes the Chat Completions tool catalog embedded by the Rust port.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/docker/docker-agent/pkg/memory/database/inmemory"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/calculator"
	"github.com/docker/docker-agent/pkg/tools/builtin/datetime"
	"github.com/docker/docker-agent/pkg/tools/builtin/fetch"
	"github.com/docker/docker-agent/pkg/tools/builtin/handoff"
	"github.com/docker/docker-agent/pkg/tools/builtin/memory"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	"github.com/docker/docker-agent/pkg/tools/builtin/random"
	"github.com/docker/docker-agent/pkg/tools/builtin/sessioncontext"
	"github.com/docker/docker-agent/pkg/tools/builtin/think"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
	"github.com/docker/docker-agent/pkg/tools/builtin/userprompt"
)

func main() {
	if err := generate(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(ctx context.Context, w io.Writer) error {
	type entry struct {
		Instructions string           `json:"instructions"`
		ReadOnly     map[string]bool  `json:"read_only"`
		Strict       map[string]bool  `json:"strict"`
		Tools        []map[string]any `json:"tools"`
	}
	catalog := make(map[string]entry)
	for key, ts := range map[string]tools.ToolSet{
		"calculator": calculator.New(), "think": think.New(), "todo": todo.New(), "user_prompt": userprompt.New(),
		"handoff": handoff.New(), "transfer_task": transfertask.New(),
		"datetime": datetime.New(), "random": random.New(), "session_context": sessioncontext.New(), "fetch": fetch.New(),
		"memory": memory.New(inmemory.New()),
		"plan":   teamloader.WithToolsExcludeFilter(plan.New(plan.WithStorage(plan.NewMemoryStorage())), plan.ToolNameUpdatePlanFromFile, plan.ToolNameExportPlanToFile),
	} {
		defs, err := ts.Tools(ctx)
		if err != nil {
			return err
		}
		e := entry{Instructions: tools.GetInstructions(ts), Strict: make(map[string]bool), ReadOnly: make(map[string]bool)}
		if key == "memory" {
			e.Instructions = strings.Replace(e.Instructions, "You have persistent memory that survives across sessions.", "You have memory scoped to this session: it survives turns and a restart of the session, but is lost when the session is closed, a new session is created or the page is reloaded.", 1)
		}
		for _, t := range defs {
			schema, strict, err := openai.ConvertParametersToSchema(t.Parameters)
			if err != nil {
				return fmt.Errorf("%s: %w", t.Name, err)
			}
			tools.OrderedSchemaProperties(schema)
			e.Strict[t.Name] = strict
			e.ReadOnly[t.Name] = t.Annotations.ReadOnlyHint
			e.Tools = append(e.Tools, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": schema,
			}})
		}
		catalog[key] = e
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(catalog)
}
