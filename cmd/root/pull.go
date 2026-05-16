package root

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/content"
	"github.com/docker/docker-agent/pkg/remote"
	"github.com/docker/docker-agent/pkg/telemetry"
)

type pullFlags struct {
	force bool
}

func newPullCmd() *cobra.Command {
	var flags pullFlags

	cmd := &cobra.Command{
		Use:   "pull <registry-ref>",
		Short: "Pull an agent from an OCI registry",
		Long:  "Pull an agent configuration file from an OCI registry",
		Args:  cobra.ExactArgs(1),
		RunE:  flags.runPullCommand,
	}

	cmd.PersistentFlags().BoolVar(&flags.force, "force", false, "Force pull even if the configuration already exists locally")

	return cmd
}

func (f *pullFlags) runPullCommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "share", append([]string{"pull"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(ctx, "share", append([]string{"pull"}, args...), commandErr)
	}()

	out := cli.NewPrinter(cmd.OutOrStdout())
	registryRef := args[0]
	slog.DebugContext(ctx, "Starting pull", "registry_ref", registryRef)

	out.Println("Pulling agent", registryRef)

	_, err := remote.Pull(ctx, registryRef, f.force)
	if err != nil {
		return fmt.Errorf("failed to pull artifact: %w", err)
	}

	store, err := content.NewStore()
	if err != nil {
		return fmt.Errorf("failed to open content store: %w", err)
	}
	yamlFile, err := store.GetArtifact(registryRef)
	if err != nil {
		return fmt.Errorf("failed to get agent yaml: %w", err)
	}

	agentName := strings.ReplaceAll(registryRef, "/", "_")
	fileName := agentName + ".yaml"

	if err := os.WriteFile(fileName, []byte(yamlFile), 0o644); err != nil { //nolint:gosec // pulled agent yaml is meant to be readable
		return err
	}

	out.Printf("Agent saved to %s\n", fileName)

	return nil
}
