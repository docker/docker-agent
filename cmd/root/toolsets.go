package root

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	loadertoolsets "github.com/docker/docker-agent/pkg/teamloader/toolsets"
	"github.com/docker/docker-agent/pkg/telemetry"
)

type toolsetsFlags struct {
	format string
}

func newToolsetsCmd() *cobra.Command {
	var flags toolsetsFlags

	cmd := &cobra.Command{
		Use:   "toolsets",
		Short: "List built-in toolset types",
		Long: `List the built-in toolset types available for use in an agent configuration.

Each type can be referenced under 'toolsets:' in an agent YAML file, for example:

  toolsets:
    - type: filesystem
    - type: shell`,
		GroupID: "diagnose",
		Args:    cobra.NoArgs,
		Example: `  docker-agent toolsets
  docker-agent toolsets --format json`,
		RunE: flags.run,
	}

	cmd.Flags().StringVar(&flags.format, "format", "table", "Output format: table, json")

	return cmd
}

func (f *toolsetsFlags) run(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "toolsets", args)
	defer func() {
		telemetry.TrackCommandError(ctx, "toolsets", args, commandErr)
	}()

	switch f.format {
	case "table":
		f.renderTable(cmd)
		return nil
	case "json":
		return f.renderJSON(cmd)
	default:
		return fmt.Errorf("unknown format %q: must be %q or %q", f.format, "table", "json")
	}
}

func (f *toolsetsFlags) renderTable(cmd *cobra.Command) {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 3, ' ', 0)
	fmt.Fprintln(w, "TYPE\tSUMMARY")
	for _, ts := range loadertoolsets.BuiltinToolsets {
		fmt.Fprintf(w, "%s\t%s\n", ts.Type, ts.Summary)
	}
	w.Flush()
}

func (f *toolsetsFlags) renderJSON(cmd *cobra.Command) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(loadertoolsets.BuiltinToolsets)
}
