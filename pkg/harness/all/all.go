// Package all imports all built-in harness adapters so their init() functions
// run and register them with the harness registry. Import this package from
// any binary that needs harness support.
package all

import (
	_ "github.com/docker/docker-agent/pkg/harness/claude"
	_ "github.com/docker/docker-agent/pkg/harness/codex"
	_ "github.com/docker/docker-agent/pkg/harness/copilot"
	_ "github.com/docker/docker-agent/pkg/harness/openclaw"
	_ "github.com/docker/docker-agent/pkg/harness/opencode"
)
