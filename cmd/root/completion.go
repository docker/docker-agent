package root

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func completeRunExec(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		return completeAlias(toComplete)
	case 1:
		return completeMessage(cmd, args, toComplete)
	default:
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

func completeAlias(toComplete string) ([]string, cobra.ShellCompDirective) {
	if strings.HasPrefix(toComplete, "/") || strings.HasPrefix(toComplete, ".") {
		return completeAgentFilename(toComplete)
	}

	var candidates []string

	// Add matching built-in agent names
	for _, name := range config.BuiltinAgentNames() {
		if strings.HasPrefix(name, toComplete) {
			candidates = append(candidates, name+"\tbuilt-in agent")
		}
	}

	// Add matching aliases
	cfg, err := userconfig.Load()
	if err == nil {
		for k, v := range cfg.Aliases {
			if strings.HasPrefix(k, toComplete) {
				candidates = append(candidates, k+"\t"+v.Path)
			}
		}
	}

	// Also add matching YAML files from the current directory
	fileCandidates, _ := completeAgentFilename(toComplete)
	candidates = append(candidates, fileCandidates...)

	return candidates, cobra.ShellCompDirectiveNoFileComp
}

func completeMessage(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if !strings.HasPrefix(toComplete, "/") {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	agentSource, err := config.Resolve(args[0], nil)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	cfg, err := config.Load(context.Background(), agentSource)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	agent, _ := cmd.Flags().GetString("agent")
	if agent == "" {
		if len(cfg.Agents) == 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		// Mirror team.DefaultAgent: prefer "root" when present, otherwise
		// the first agent declared. This keeps shell completion in sync
		// with the agent the runtime would actually run.
		agent = cfg.Agents[0].Name
		if _, hasRoot := cfg.Agents.Lookup("root"); hasRoot {
			agent = "root"
		}
	}
	agentCfg, found := cfg.Agents.Lookup(agent)
	if !found {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	var candidates []string
	for k, v := range agentCfg.Commands {
		if strings.HasPrefix("/"+k, toComplete) {
			candidates = append(candidates, "/"+k+"\t"+v.DisplayText())
		}
	}

	return candidates, cobra.ShellCompDirectiveNoFileComp
}

func completeAgentFilename(toComplete string) ([]string, cobra.ShellCompDirective) {
	dirPrefix, base := filepath.Split(toComplete)

	dirToRead := dirPrefix
	if dirToRead == "" {
		dirToRead = "."
	}

	entries, err := os.ReadDir(dirToRead)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base) {
			continue
		}

		switch {
		case e.IsDir():
			out = append(out, dirPrefix+name+string(filepath.Separator))
		case strings.EqualFold(filepath.Ext(name), ".yaml"), strings.EqualFold(filepath.Ext(name), ".yml"):
			out = append(out, dirPrefix+name)
		}
	}

	// Don't add space after single directory completion
	if len(out) == 1 && strings.HasSuffix(out[0], string(filepath.Separator)) {
		return out, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	}

	return out, cobra.ShellCompDirectiveNoFileComp
}
