package hooks

import "errors"

// ErrHookTemplatesDisabled is returned for hooks whose arguments are Go
// templates (add_context, type: model) when the binary is built with
// -tags docker_agent_no_hook_templates. The tag drops text/template, whose
// reflective method lookups stop the linker from pruning unused methods;
// size-sensitive embedders such as WASM builds use it.
var ErrHookTemplatesDisabled = errors.New("template-based hooks are not available in this build (docker_agent_no_hook_templates)")
