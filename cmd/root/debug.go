package root

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"

	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

type debugFlags struct {
	modelOverrides []string
	toolsetsJSON   bool
	skillsJSON     bool
	runConfig      config.RuntimeConfig
}

// Explicit DTOs keep the JSON contract stable and independent of internal types.
type agentToolsInfo struct {
	Agent string     `json:"agent"`
	Tools []toolInfo `json:"tools"`
}

type toolInfo struct {
	Name         string                `json:"name"`
	Category     string                `json:"category"`
	Description  string                `json:"description"`
	Parameters   any                   `json:"parameters"`
	Annotations  tools.ToolAnnotations `json:"annotations"`
	OutputSchema any                   `json:"outputSchema"`
}

type agentSkillsInfo struct {
	Agent  string      `json:"agent"`
	Skills []skillInfo `json:"skills"`
}

type skillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Forked      bool   `json:"forked"`
	Path        string `json:"path,omitempty"`
}

func newDebugCmd() *cobra.Command {
	var flags debugFlags

	cmd := &cobra.Command{
		Use:     "debug",
		Short:   "Debug tools",
		GroupID: "advanced",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "config <agent-file>|<registry-ref> [flavor...]",
		Short: "Print the canonical form of an agent's configuration file",
		Long: "Print the canonical form of an agent's configuration file.\n\n" +
			"When flavors are given (positionally or with --flavor), they are applied in order and the\n" +
			"resolved config is printed without its 'flavors' section, since the patches are already baked in.",
		Args: cobra.MinimumNArgs(1),
		RunE: flags.runDebugConfigCommand,
	})
	toolsetsCmd := &cobra.Command{
		Use:   "toolsets <agent-file>|<registry-ref>",
		Short: "Debug the toolsets of an agent",
		Args:  cobra.ExactArgs(1),
		RunE:  flags.runDebugToolsetsCommand,
	}
	toolsetsCmd.Flags().BoolVar(&flags.toolsetsJSON, "json", false, "Output in JSON format")
	cmd.AddCommand(toolsetsCmd)
	skillsCmd := &cobra.Command{
		Use:   "skills <agent-file>|<registry-ref>",
		Short: "Debug the skills of an agent",
		Args:  cobra.ExactArgs(1),
		RunE:  flags.runDebugSkillsCommand,
	}
	skillsCmd.Flags().BoolVar(&flags.skillsJSON, "json", false, "Output in JSON format")
	cmd.AddCommand(skillsCmd)
	titleCmd := &cobra.Command{
		Use:   "title <agent-file>|<registry-ref> <question>",
		Short: "Generate a session title from a question",
		Args:  cobra.ExactArgs(2),
		RunE:  flags.runDebugTitleCommand,
	}
	titleCmd.Flags().StringArrayVar(&flags.modelOverrides, "model", nil, "Override agent model: [agent=]provider/model (repeatable)")
	cmd.AddCommand(titleCmd)

	addRuntimeConfigFlags(cmd, &flags.runConfig)

	cmd.AddCommand(newDebugAuthCmd())
	cmd.AddCommand(newDebugOAuthCmd())

	return cmd
}

// loadTeam loads an agent team from the given agent file.
// Callers should defer stopToolSets(ctx, t) to clean up.
func (f *debugFlags) loadTeam(ctx context.Context, agentFilename string, opts ...teamloader.Opt) (*team.Team, error) {
	agentSource, err := sources.Resolve(agentFilename, f.runConfig.EnvProvider())
	if err != nil {
		return nil, err
	}

	opts = append(loaderdefaults.Opts(), opts...)
	t, err := teamloader.Load(ctx, agentSource, &f.runConfig, opts...)
	if err != nil {
		return nil, err
	}

	return t, nil
}

func (f *debugFlags) runDebugConfigCommand(cmd *cobra.Command, args []string) (commandErr error) {
	telemetry.TrackCommand(cmd.Context(), "debug", append([]string{"config"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(cmd.Context(), "debug", append([]string{"config"}, args...), commandErr)
	}()

	agentSource, err := sources.Resolve(args[0], f.runConfig.EnvProvider())
	if err != nil {
		return err
	}

	flavors := append(slices.Clone(f.runConfig.Flavors), args[1:]...)
	cfg, err := config.Load(cmd.Context(), agentSource, config.WithFlavors(flavors...))
	if err != nil {
		return err
	}

	// Once resolved, re-applying a flavor to the output would double-apply
	// `+`/`-` patches, so the section is dropped.
	if len(flavors) > 0 {
		cfg.Flavors = nil
	}

	return yaml.NewEncoder(cmd.OutOrStdout()).Encode(cfg)
}

func (f *debugFlags) runDebugToolsetsCommand(cmd *cobra.Command, args []string) (commandErr error) {
	telemetry.TrackCommand(cmd.Context(), "debug", append([]string{"toolsets"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(cmd.Context(), "debug", append([]string{"toolsets"}, args...), commandErr)
	}()

	ctx := cmd.Context()

	t, err := f.loadTeam(ctx, args[0])
	if err != nil {
		return err
	}
	defer stopToolSets(ctx, t)

	out := cli.NewPrinter(cmd.OutOrStdout())
	var infos []agentToolsInfo

	for _, name := range t.AgentNames() {
		agent, err := t.Agent(name)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to get agent", "name", name, "error", err)
			continue
		}

		agentTools, err := agent.Tools(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to query tools", "name", agent.Name(), "error", err)
			continue
		}

		info := agentToolsInfo{Agent: agent.Name(), Tools: make([]toolInfo, 0, len(agentTools))}
		for _, tool := range agentTools {
			info.Tools = append(info.Tools, toolInfo{
				Name:         tool.Name,
				Category:     tool.Category,
				Description:  tool.Description,
				Parameters:   tool.Parameters,
				Annotations:  tool.Annotations,
				OutputSchema: tool.OutputSchema,
			})
		}

		// Text mode streams per agent so slow toolsets don't hold back earlier results.
		if f.toolsetsJSON {
			infos = append(infos, info)
			continue
		}
		printAgentTools(out, info)
	}

	if f.toolsetsJSON {
		return encodeJSON(cmd, infos)
	}
	return nil
}

func printAgentTools(out *cli.Printer, info agentToolsInfo) {
	if len(info.Tools) == 0 {
		out.Printf("No tools for %s\n", info.Agent)
		return
	}

	out.Printf("%d tool(s) for %s:\n", len(info.Tools), info.Agent)
	for _, tool := range info.Tools {
		out.Println(" +", tool.Name, "-", tool.Description)
	}
}

func (f *debugFlags) runDebugSkillsCommand(cmd *cobra.Command, args []string) (commandErr error) {
	telemetry.TrackCommand(cmd.Context(), "debug", append([]string{"skills"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(cmd.Context(), "debug", append([]string{"skills"}, args...), commandErr)
	}()

	ctx := cmd.Context()

	t, err := f.loadTeam(ctx, args[0])
	if err != nil {
		return err
	}
	defer stopToolSets(ctx, t)

	out := cli.NewPrinter(cmd.OutOrStdout())
	var infos []agentSkillsInfo

	for _, name := range t.AgentNames() {
		agent, err := t.Agent(name)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to get agent", "name", name, "error", err)
			continue
		}

		info := agentSkillsInfo{Agent: agent.Name(), Skills: []skillInfo{}}
		// The loader creates at most one skills toolset per agent.
		for _, ts := range agent.ToolSets() {
			st, ok := tools.As[*skillstool.ToolSet](ts)
			if !ok {
				continue
			}
			for _, skill := range st.Skills() {
				info.Skills = append(info.Skills, skillInfo{
					Name:        skill.Name,
					Description: skill.Description,
					Forked:      skill.IsFork(),
					Path:        skill.FilePath,
				})
			}
			break
		}

		if f.skillsJSON {
			infos = append(infos, info)
			continue
		}
		printAgentSkills(out, info)
	}

	if f.skillsJSON {
		return encodeJSON(cmd, infos)
	}
	return nil
}

func printAgentSkills(out *cli.Printer, info agentSkillsInfo) {
	if len(info.Skills) == 0 {
		out.Printf("No skills for %s\n", info.Agent)
		return
	}

	out.Printf("%d skill(s) for %s:\n", len(info.Skills), info.Agent)
	for _, skill := range info.Skills {
		marker := ""
		if skill.Forked {
			marker = " [forked]"
		}
		out.Println(" +", skill.Name+marker, "-", skill.Description)
	}
}

func encodeJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (f *debugFlags) runDebugTitleCommand(cmd *cobra.Command, args []string) (commandErr error) {
	telemetry.TrackCommand(cmd.Context(), "debug", append([]string{"title"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(cmd.Context(), "debug", append([]string{"title"}, args...), commandErr)
	}()

	ctx := cmd.Context()

	t, err := f.loadTeam(ctx, args[0], teamloader.WithModelOverrides(f.modelOverrides))
	if err != nil {
		return err
	}
	defer stopToolSets(ctx, t)

	agent, err := t.DefaultAgent()
	if err != nil {
		return err
	}

	// Use the same title generation code path as the TUI (see runTUI in new.go),
	// including any dedicated title_model configured for the agent's model.
	models := agent.TitleModels(ctx)
	if len(models) == 0 {
		return fmt.Errorf("agent %q has no model configured", agent.Name())
	}
	gen := sessiontitle.New(models...)
	if gen == nil {
		return fmt.Errorf("agent %q has no usable title model", agent.Name())
	}

	title, err := gen.Generate(ctx, "debug", []string{args[1]})
	if err != nil {
		return fmt.Errorf("generating title with agent %q: %w", agent.Name(), err)
	}

	fmt.Fprintln(cmd.OutOrStdout(), title)

	return nil
}
