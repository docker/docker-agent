package config

import (
	"fmt"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// resolveRAGDefinitions resolves RAG definition references in agent toolsets.
// When an agent toolset of type "rag" has a ref that matches a key in the
// top-level rag section, the toolset is expanded with the definition's properties.
// Any properties set directly on the toolset override the definition properties.
func resolveRAGDefinitions(cfg *latest.Config) error {
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		for j := range agent.Toolsets {
			ts := &agent.Toolsets[j]
			if ts.Type != "rag" || ts.Ref == "" {
				continue
			}

			def, ok := cfg.RAG[ts.Ref]
			if !ok {
				return fmt.Errorf("agent '%s' references non-existent RAG definition '%s'", agent.Name, ts.Ref)
			}

			applyRAGDefaults(ts, &def.Toolset, ts.Ref)
		}
	}

	return nil
}

// applyRAGDefaults fills empty fields in ts from def. Toolset values win.
func applyRAGDefaults(ts, def *latest.Toolset, refName string) {
	// Clear the ref since it's been resolved
	ts.Ref = ""

	if ts.RAGConfig == nil {
		ts.RAGConfig = def.RAGConfig
	}
	if ts.Instruction == "" {
		ts.Instruction = def.Instruction
	}
	if len(ts.Tools) == 0 {
		ts.Tools = def.Tools
	}
	if ts.Defer.IsEmpty() {
		ts.Defer = def.Defer
	}
	if ts.Name == "" {
		ts.Name = def.Name
	}
	// If name is still empty after applying defaults from the definition,
	// use the ref key (e.g., "rag1") so that multiple RAG tools get unique
	// tool names instead of all defaulting to "rag".
	if ts.Name == "" {
		ts.Name = refName
	}
}
