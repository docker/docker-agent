//go:build js && wasm && docker_agent_wasm_small && (!docker_agent_no_hook_templates || !docker_agent_no_js_expansion || docker_agent_wasm_cloud)

package main

// The small profile only pays off without the reflection roots, and has no
// cloud providers. Build it with:
//
//	-tags docker_agent_wasm_small,docker_agent_no_hook_templates,docker_agent_no_js_expansion
var _ = docker_agent_wasm_small_needs_tags_docker_agent_no_hook_templates_and_docker_agent_no_js_expansion_without_docker_agent_wasm_cloud
