package root

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/telemetry"
)

type modelsListFlags struct {
	providerFilter string
	format         string
	all            bool
	runConfig      config.RuntimeConfig
}

// modelRow represents a single model entry for display or serialization.
type modelRow struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Default  bool   `json:"default,omitempty"`
}

func newModelsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "List available models",
		Long: `List models available for use with --model flag.

Shows models that can be passed to 'docker agent run --model' or
'docker agent new --model'. By default shows models from providers
you have credentials for. Use --all to include all providers.`,
		GroupID: "core",
	}

	listCmd := newModelsListCmd()
	cmd.AddCommand(listCmd)

	// Default to "list" when no subcommand given.
	cmd.RunE = listCmd.RunE

	// Copy the flags from the list command so they work on the bare
	// "docker agent models --provider openai" form as well.
	cmd.Flags().AddFlagSet(listCmd.Flags())

	return cmd
}

func newModelsListCmd() *cobra.Command {
	var flags modelsListFlags

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List available models",
		Example: `  docker agent models
  docker agent models list --provider openai
  docker agent models ls --all
  docker agent models --format json`,
		Args: cobra.NoArgs,
		RunE: flags.runModelsListCommand,
	}

	cmd.Flags().StringVarP(&flags.providerFilter, "provider", "p", "", "Filter by provider name")
	cmd.Flags().StringVar(&flags.format, "format", "table", "Output format: table, json")
	cmd.Flags().BoolVarP(&flags.all, "all", "a", false, "Include models from all providers, not just those with credentials")
	addGatewayFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *modelsListFlags) runModelsListCommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "models", append([]string{"list"}, args...))
	defer func() {
		telemetry.TrackCommandError(ctx, "models", append([]string{"list"}, args...), commandErr)
	}()

	out := cli.NewPrinter(cmd.OutOrStdout())
	env := f.runConfig.EnvProvider()

	// Determine which providers the user has credentials for.
	availableProviders := make(map[string]bool)
	for _, p := range config.AvailableProviders(ctx, f.runConfig.ModelsGateway, env) {
		availableProviders[p] = true
	}

	// Determine which model auto-selection would pick.
	autoModel := config.AutoModelConfig(ctx, f.runConfig.ModelsGateway, env, f.runConfig.DefaultModel)

	rows := f.collectModels(ctx, availableProviders, autoModel)

	// Apply provider filter
	if f.providerFilter != "" {
		rows = slices.DeleteFunc(rows, func(r modelRow) bool {
			return !strings.EqualFold(r.Provider, f.providerFilter)
		})
	}

	// Sort: default first, then by provider, then by model
	slices.SortFunc(rows, func(a, b modelRow) int {
		if a.Default != b.Default {
			if a.Default {
				return -1
			}
			return 1
		}
		if c := strings.Compare(a.Provider, b.Provider); c != 0 {
			return c
		}
		return strings.Compare(a.Model, b.Model)
	})

	if len(rows) == 0 {
		out.Println("No models available.")
		out.Println("\nConfigure a provider API key or install Docker Model Runner.")
		return nil
	}

	switch f.format {
	case "json":
		return f.renderJSON(cmd, rows)
	default:
		f.renderTable(cmd, rows)
	}

	return nil
}

// collectModels returns all models from the catalog, filtered by credential
// availability unless --all is set. Default models for each available provider
// are always included even if the catalog fetch fails.
func (f *modelsListFlags) collectModels(ctx context.Context, availableProviders map[string]bool, autoModel latest.ModelConfig) []modelRow {
	seen := make(map[string]bool)
	var rows []modelRow

	// Always include the per-provider defaults so we have something even
	// if the catalog is unreachable.
	for prov, model := range config.DefaultModels {
		if !f.all && !availableProviders[prov] {
			continue
		}
		ref := prov + "/" + model
		seen[ref] = true
		rows = append(rows, modelRow{
			Provider: prov,
			Model:    model,
			Default:  prov == autoModel.Provider && model == autoModel.Model,
		})
	}

	// Fetch catalog and add all text-capable models.
	store, err := f.runConfig.ModelsDevStore()
	if err != nil {
		return rows
	}
	db, err := store.GetDatabase(ctx)
	if err != nil {
		return rows
	}

	for providerID, prov := range db.Providers {
		if !provider.IsCatalogProvider(providerID) {
			continue
		}
		if !f.all && !availableProviders[providerID] {
			continue
		}
		for modelID, model := range prov.Models {
			if !slices.Contains(model.Modalities.Output, "text") {
				continue
			}
			if isEmbeddingModel(model.Family, model.Name) {
				continue
			}

			ref := providerID + "/" + modelID
			if seen[ref] {
				continue
			}
			seen[ref] = true

			rows = append(rows, modelRow{
				Provider: providerID,
				Model:    modelID,
			})
		}
	}

	return rows
}

func isEmbeddingModel(family, name string) bool {
	fl := strings.ToLower(family)
	nl := strings.ToLower(name)
	return strings.Contains(fl, "embed") || strings.Contains(nl, "embed")
}

func (f *modelsListFlags) renderTable(cmd *cobra.Command, rows []modelRow) {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 3, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tMODEL\tDEFAULT")
	for _, r := range rows {
		def := ""
		if r.Default {
			def = "*"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.Provider, r.Model, def)
	}
	w.Flush()
}

func (f *modelsListFlags) renderJSON(cmd *cobra.Command, rows []modelRow) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}
