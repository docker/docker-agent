package root

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/content"
	"github.com/docker/docker-agent/pkg/oci"
	"github.com/docker/docker-agent/pkg/remote"
	"github.com/docker/docker-agent/pkg/telemetry"
)

func newPushCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "push <agent-file> <registry-ref>",
		Short: "Push an agent to an OCI registry",
		Long:  "Push an agent configuration file to an OCI registry",
		Args:  cobra.ExactArgs(2),
		RunE:  runPushCommand,
	}
}

func runPushCommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "share", append([]string{"push"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(ctx, "share", append([]string{"push"}, args...), commandErr)
	}()

	agentFilename := args[0]
	tag := args[1]
	out := cli.NewPrinter(cmd.OutOrStdout())

	store, err := content.NewStore()
	if err != nil {
		return err
	}

	agentSource, err := config.Resolve(agentFilename, nil)
	if err != nil {
		return fmt.Errorf("resolving agent file: %w", err)
	}

	_, err = oci.PackageFileAsOCIToStore(ctx, agentSource, tag, store)
	if err != nil {
		return fmt.Errorf("failed to build artifact: %w", err)
	}

	slog.DebugContext(ctx, "Starting push", "registry_ref", tag)

	out.Printf("Pushing agent %s to %s\n", agentFilename, tag)

	err = remote.Push(ctx, tag)
	if err != nil {
		return fmt.Errorf("failed to push artifact: %w", err)
	}

	out.Printf("Successfully pushed artifact to %s\n", tag)
	return nil
}
