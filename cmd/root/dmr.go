package root

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/docker/cli/cli/command"
	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/connhelper/commandconn"
	cliflags "github.com/docker/cli/cli/flags"
	"github.com/spf13/pflag"

	"github.com/docker/docker-agent/pkg/model/provider/dmr/dmrmodels"
)

func withDMRDockerConnection(ctx context.Context, dockerCLI command.Cli, flags *pflag.FlagSet) (context.Context, error) {
	args, err := dmrDockerArgs(ctx, dockerCLI, flags)
	if err != nil {
		return nil, err
	}

	// Delegate socket, TLS, SSH and named-pipe handling to the same Docker CLI
	// that runs model status. No engine connection is opened until HTTP dials.
	dialArgs := slices.Concat(args, []string{"system", "dial-stdio"})
	return dmrmodels.ContextWithDockerConnection(ctx, args, func(ctx context.Context) (net.Conn, error) {
		return commandconn.New(ctx, "docker", dialArgs...)
	}), nil
}

func dmrDockerArgs(ctx context.Context, dockerCLI command.Cli, flags *pflag.FlagSet) ([]string, error) {
	if flags == nil {
		flags = pflag.NewFlagSet("docker", pflag.ContinueOnError)
		cliflags.NewClientOptions().InstallFlags(flags)
	}

	// Resolve paths before --working-dir changes the process directory.
	configDir, err := filepath.Abs(cmp.Or(flags.Lookup("config").Value.String(), dockerconfig.Dir()))
	if err != nil {
		return nil, fmt.Errorf("resolving Docker config directory: %w", err)
	}
	args := []string{"--config=" + configDir}
	for _, name := range []string{"tls", "tlsverify"} {
		if flag := flags.Lookup(name); flag.Changed {
			args = append(args, "--"+name+"="+flag.Value.String())
		}
	}
	contextName, _ := flags.GetString("context")
	if contextName == "" && !flags.Changed("host") {
		switch {
		case dockerCLI != nil:
			contextName = dockerCLI.CurrentContext()
		case os.Getenv("DOCKER_HOST") != "":
			contextName = command.DefaultContextName
		case os.Getenv("DOCKER_CONTEXT") != "":
			contextName = os.Getenv("DOCKER_CONTEXT")
		default:
			cfg, err := dockerconfig.Load(configDir)
			if err != nil {
				// Docker itself tolerates an unreadable config for connection selection.
				slog.DebugContext(ctx, "Failed to load Docker config", "error", err)
			}
			contextName = cfg.CurrentContext
		}
	}
	if flags.Changed("host") || (contextName == command.DefaultContextName && os.Getenv("DOCKER_HOST") != "") {
		host := os.Getenv("DOCKER_HOST")
		if flags.Changed("host") {
			host = flags.Lookup("host").Value.String()
		}
		if socket, ok := strings.CutPrefix(strings.TrimSpace(host), "unix://"); ok && socket != "" {
			path, err := filepath.Abs(socket)
			if err != nil {
				return nil, fmt.Errorf("resolving Docker socket: %w", err)
			}
			host = "unix://" + path
		}
		args = append(args, "--host="+host)
	} else {
		args = append(args, "--context="+cmp.Or(contextName, command.DefaultContextName))
	}

	tls, _ := flags.GetBool("tls")
	tlsVerify, _ := flags.GetBool("tlsverify")
	for _, name := range []string{"tlscacert", "tlscert", "tlskey"} {
		flag := flags.Lookup(name)
		if !flag.Changed && !tls && !tlsVerify && !flags.Changed("tlsverify") {
			continue
		}
		path := flag.Value.String()
		// Docker ignores missing default client certificates, but not explicit ones.
		if !flag.Changed && name != "tlscacert" {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				path = ""
			}
		}
		if path != "" {
			path, err = filepath.Abs(path)
			if err != nil {
				return nil, fmt.Errorf("resolving Docker --%s: %w", name, err)
			}
		}
		args = append(args, "--"+name+"="+path)
	}
	return args, nil
}
