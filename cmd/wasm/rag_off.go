//go:build js && wasm && !docker_agent_wasm_rag

package main

import (
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/teamloader"
)

func registerRAG(map[string]teamloader.ToolsetCreator, map[string][]byte) {}

func checkRAG(latest.Toolset, map[string][]byte) error {
	// Strict loading rejects the unregistered toolset before model construction.
	return nil
}
