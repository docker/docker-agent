// Command gen writes the Chat Completions tool catalog embedded by the Rust port.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/calculator"
	"github.com/docker/docker-agent/pkg/tools/builtin/think"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tools/builtin/userprompt"
)

func main() {
	if err := generate(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(ctx context.Context, w io.Writer) error {
	type toolset interface {
		tools.ToolSet
		Instructions() string
	}
	type entry struct {
		Instructions string           `json:"instructions"`
		Strict       map[string]bool  `json:"strict"`
		Tools        []map[string]any `json:"tools"`
	}
	catalog := make(map[string]entry)
	for key, ts := range map[string]toolset{"calculator": calculator.New(), "think": think.New(), "todo": todo.New(), "user_prompt": userprompt.New()} {
		defs, err := ts.Tools(ctx)
		if err != nil {
			return err
		}
		e := entry{Instructions: ts.Instructions(), Strict: make(map[string]bool)}
		for _, t := range defs {
			schema, strict, err := openai.ConvertParametersToSchema(t.Parameters)
			if err != nil {
				return fmt.Errorf("%s: %w", t.Name, err)
			}
			tools.OrderedSchemaProperties(schema)
			e.Strict[t.Name] = strict
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
