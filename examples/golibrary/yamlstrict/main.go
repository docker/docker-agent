// Example: load an agent from YAML (a local file or an OCI reference) while
// linking only the providers, toolsets and agent sources this binary needs.
//
// The default registries in pkg/teamloader/defaults pull every provider SDK
// and every built-in toolset. Here the registries are hand-picked, and
// teamloader.WithStrict makes the load fail up front — listing every offending
// item — if the YAML asks for anything else.
//
//	go run ./examples/golibrary/yamlstrict ./agent.yaml
//	go run ./examples/golibrary/yamlstrict myorg/agent:latest
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/ocisource"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools/builtin/api"
	"github.com/docker/docker-agent/pkg/tools/builtin/fetch"
	"github.com/docker/docker-agent/pkg/tools/builtin/think"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: yamlstrict <agent.yaml | oci-reference>")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, os.Args[1]); err != nil {
		log.Println(err)
	}
}

func run(ctx context.Context, ref string) error {
	// Pick the source type explicitly; each lives in its own package so only
	// the one you import is linked. pkg/config/sources resolves any kind of
	// reference (files, directories, URLs, OCI, aliases) at the cost of
	// linking all of them.
	var source config.Source
	if config.IsOCIReference(ref) {
		source = ocisource.New(ref)
	} else {
		source = config.NewFileSource(ref)
	}

	providers := provider.NewRegistry(map[string]provider.Factory{
		"anthropic": provider.Adapt(anthropic.NewClient),
		"openai":    provider.Adapt(openai.NewClient),
	})
	toolsets := teamloader.NewToolsetRegistry(map[string]teamloader.ToolsetCreator{
		"api":   api.Creator,
		"fetch": fetch.Creator,
		"think": teamloader.Creator(think.CreateToolSet),
	})

	runConfig := &config.RuntimeConfig{}
	team, err := teamloader.Load(ctx, source, runConfig,
		teamloader.WithProviderRegistry(providers),
		teamloader.WithToolsetRegistry(toolsets),
		// Reject anything else the YAML relies on: other provider types,
		// other toolset types, and every optional feature (hooks, skills,
		// harness, external sub-agents, code mode, ...). Third-party YAML
		// gets exactly the capabilities listed above, nothing more. Allow a
		// feature explicitly, e.g. WithStrict(config.FeatureSkills), only
		// after weighing what it lets the config reach on the host.
		teamloader.WithStrict(),
	)
	if err != nil {
		var unsupported *config.UnsupportedError
		if errors.As(err, &unsupported) {
			return fmt.Errorf("this binary cannot run %s:\n%w", ref, err)
		}
		return err
	}

	rt, err := runtime.New(ctx, team)
	if err != nil {
		return err
	}

	messages, err := rt.Run(ctx, session.New(session.WithUserMessage("Introduce yourself in one sentence.")))
	if err != nil {
		return err
	}
	fmt.Println(messages[len(messages)-1].Message.Content)
	return nil
}
