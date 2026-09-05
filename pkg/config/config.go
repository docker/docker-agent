package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// LoadOption customizes a single Load call.
type LoadOption func(*loadOptions)

type loadOptions struct {
	flavors []string
}

// WithFlavors enables the named flavor patches for this load. Order matters:
// patches are applied in the order given. Names the config does not define
// are ignored with a debug log.
func WithFlavors(names ...string) LoadOption {
	return func(o *loadOptions) {
		o.flavors = append(o.flavors, names...)
	}
}

func Load(ctx context.Context, source Source, opts ...LoadOption) (*latest.Config, error) {
	var options loadOptions
	for _, opt := range opts {
		opt(&options)
	}

	data, err := source.Read(ctx)
	if err != nil {
		return nil, err
	}

	// Flavor patches rewrite the raw document, so they run before anything
	// (including the version sniff below) reads it.
	if data, err = applyFlavors(ctx, data, options.flavors); err != nil {
		return nil, err
	}

	var raw struct {
		Version string `yaml:"version,omitempty"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("looking for version in config file\n%s", yaml.FormatError(err, true, true))
	}
	raw.Version = cmp.Or(raw.Version, latest.Version)

	oldConfig, err := parseCurrentVersion(data, raw.Version)
	if err != nil {
		msg := yaml.FormatError(err, true, true)
		if hint := newerVersionHint(data, raw.Version, err); hint != "" {
			msg += "\n" + hint
		} else if hint := removedFieldHint(raw.Version, err); hint != "" {
			msg += "\n" + hint
		}
		return nil, fmt.Errorf("parsing config file\n%s", msg)
	}

	config, err := migrateToLatestConfig(oldConfig, data)
	if err != nil {
		return nil, fmt.Errorf("migrating config: %w", err)
	}

	config.Version = raw.Version

	if err := resolveInstructionFiles(&config, source); err != nil {
		return nil, err
	}

	if err := validateConfig(&config); err != nil {
		return nil, err
	}

	warnExpansionMismatches(ctx, slog.Default(), &config)
	warnMaxTokensVsContextWindow(ctx, slog.Default(), &config)

	return &config, nil
}

// resolveInstructionFiles replaces every agent's instruction_file reference
// with the file's contents, loaded relative to the config file's directory.
// When several files are listed their contents are concatenated in order,
// separated by a blank line. Resolution happens once at load time so the rest
// of the pipeline (and any marshalled/pushed copy of the config) only ever
// sees the inlined Instruction; the InstructionFile field is cleared
// afterwards to keep the loaded config self-contained.
//
// Each reference must be a local relative path inside the config directory:
// absolute paths and "../" traversal are rejected, and reads are confined to
// the directory with os.OpenRoot so symlinks cannot escape it. This mirrors
// the path-safety rules used by the HCL file() helper and fileSource.Read.
func resolveInstructionFiles(cfg *latest.Config, source Source) error {
	parentDir := source.ParentDir()

	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if len(agent.InstructionFile) == 0 {
			continue
		}
		if agent.Instruction != "" {
			return fmt.Errorf("agent %q: 'instruction' and 'instruction_file' are mutually exclusive, set only one", agent.Name)
		}
		if parentDir == "" {
			return fmt.Errorf("agent %q: 'instruction_file' is only supported for local file-based configs, not OCI/URL sources", agent.Name)
		}

		instruction, err := readInstructionFiles(parentDir, agent.InstructionFile)
		if err != nil {
			return fmt.Errorf("agent %q: %w", agent.Name, err)
		}

		agent.Instruction = instruction
		agent.InstructionFile = nil
	}

	return nil
}

// readInstructionFiles loads each path (resolved inside parentDir with
// os.OpenRoot so symlinks cannot escape) and returns their contents joined by
// a blank line. Each path must be a local relative path: absolute paths and
// "../" traversal are rejected.
func readInstructionFiles(parentDir string, paths []string) (string, error) {
	root, err := os.OpenRoot(parentDir)
	if err != nil {
		return "", fmt.Errorf("opening config directory %q: %w", parentDir, err)
	}
	defer root.Close()

	parts := make([]string, 0, len(paths))
	for _, path := range paths {
		if !filepath.IsLocal(path) {
			return "", fmt.Errorf("instruction_file %q must be a local relative path inside the config directory", path)
		}
		data, err := root.ReadFile(filepath.ToSlash(path))
		if err != nil {
			return "", fmt.Errorf("reading instruction_file %q: %w", path, err)
		}
		parts = append(parts, string(data))
	}

	return strings.Join(parts, "\n\n"), nil
}

// CheckRequiredEnvVars checks which environment variables are required by the models and tools.
//
// This allows exiting early with a proper error message instead of failing later when trying to use a model or tool.
func CheckRequiredEnvVars(ctx context.Context, cfg *latest.Config, modelsGateway string, env environment.Provider) error {
	if modelsGateway != "" && environment.IsTrustedDockerURL(modelsGateway) {
		if jwt, _ := env.Get(ctx, environment.DockerDesktopTokenEnv); jwt == "" {
			return errors.New("sorry, you first need to sign in Docker Desktop to use the Docker AI Gateway")
		}
	}

	missing, missingModelCreds, err := gatherMissingEnvVars(ctx, cfg, modelsGateway, env)
	if err != nil {
		// If there's a tool preflight error, log it but continue
		slog.WarnContext(ctx, "Failed to preflight toolset environment variables; continuing", "error", err)
	}

	// Return error if there are missing environment variables
	if len(missing) > 0 {
		return &environment.RequiredEnvError{
			Missing:                 missing,
			MissingModelCredentials: missingModelCreds,
		}
	}

	return nil
}

func parseCurrentVersion(data []byte, version string) (any, error) {
	parsers, _ := versions()
	parser, found := parsers[version]
	if !found {
		return nil, fmt.Errorf("unsupported config version: %v (valid versions: %s)", version, strings.Join(slices.Sorted(maps.Keys(parsers)), ", "))
	}
	return parser(data)
}

// newerVersionHint returns a hint when a parse error is caused by a key or a
// value shape that a newer config version accepts (an unknown field, or a type
// mismatch such as a list where an older schema only takes a string), so the
// user is pointed at the `version` bump instead of a generic YAML error. It
// tries the parsers for every version above the declared one, in order, and
// points the user at the smallest version that parses the config successfully.
// Best-effort: a newer version may accept the config for unrelated reasons
// (laxer schema), so the original unknown-field error is always shown before
// the hint.
func newerVersionHint(data []byte, version string, parseErr error) string {
	var unknownField *yaml.UnknownFieldError
	var typeErr *yaml.TypeError
	if !errors.As(parseErr, &unknownField) && !errors.As(parseErr, &typeErr) {
		return ""
	}

	current, err := strconv.Atoi(version)
	if err != nil {
		return ""
	}

	parsers, _ := versions()
	var newer []int
	for v := range parsers {
		if n, err := strconv.Atoi(v); err == nil && n > current {
			newer = append(newer, n)
		}
	}
	slices.Sort(newer)

	for _, n := range newer {
		v := strconv.Itoa(n)
		if _, err := parsers[v](data); err == nil {
			return fmt.Sprintf("hint: this syntax is supported by config version %s; update the top-level 'version' field (currently %s)", v, version)
		}
	}

	return ""
}

func migrateToLatestConfig(c any, raw []byte) (latest.Config, error) {
	var err error

	_, upgraders := versions()
	for _, upgrade := range upgraders {
		c, err = upgrade(c, raw)
		if err != nil {
			return latest.Config{}, err
		}
	}

	return c.(latest.Config), nil
}

func validateConfig(cfg *latest.Config) error {
	if len(cfg.Agents) == 0 {
		return errors.New("at least one agent must be configured (add an entry under 'agents')")
	}

	if err := validateProviders(cfg); err != nil {
		return err
	}

	if cfg.Models == nil {
		cfg.Models = map[string]latest.ModelConfig{}
	}

	for name := range cfg.Models {
		if cfg.Models[name].ParallelToolCalls == nil {
			m := cfg.Models[name]
			m.ParallelToolCalls = new(true)
			cfg.Models[name] = m
		}
	}

	if err := ensureModelsExist(cfg); err != nil {
		return err
	}

	if err := resolveToolsetDefinitions(cfg); err != nil {
		return err
	}

	if err := resolveMCPDefinitions(cfg); err != nil {
		return err
	}

	if err := resolveRAGDefinitions(cfg); err != nil {
		return err
	}

	if err := resolveCommandDefinitions(cfg); err != nil {
		return err
	}

	if err := resolveSkillDefinitions(cfg); err != nil {
		return err
	}

	if err := validateSkillToolsetRefs(cfg); err != nil {
		return err
	}

	allNames := map[string]bool{}
	for _, agent := range cfg.Agents {
		allNames[agent.Name] = true
	}

	for _, agent := range cfg.Agents {
		for _, subAgentRef := range agent.SubAgents {
			if _, exists := allNames[subAgentRef]; !exists && !IsExternalReference(subAgentRef) {
				return fmt.Errorf("agent '%s' references non-existent sub-agent '%s'", agent.Name, subAgentRef)
			}
			if IsExternalReference(subAgentRef) {
				name, _ := ParseExternalAgentRef(subAgentRef)
				if allNames[name] {
					return fmt.Errorf("agent '%s': external sub-agent '%s' resolves to name '%s' which conflicts with a locally-defined agent", agent.Name, subAgentRef, name)
				}
			}
		}

		for _, handoffRef := range agent.Handoffs {
			if _, exists := allNames[handoffRef]; !exists && !IsExternalReference(handoffRef) {
				return fmt.Errorf("agent '%s' references non-existent handoff agent '%s'", agent.Name, handoffRef)
			}
			if IsExternalReference(handoffRef) {
				name, _ := ParseExternalAgentRef(handoffRef)
				if allNames[name] {
					return fmt.Errorf("agent '%s': external handoff '%s' resolves to name '%s' which conflicts with a locally-defined agent", agent.Name, handoffRef, name)
				}
			}
		}

		if err := validateSkills(fmt.Sprintf("agent '%s'", agent.Name), &agent.Skills); err != nil {
			return err
		}
	}

	if err := validateForceHandoffs(cfg, allNames); err != nil {
		return err
	}

	return nil
}

// validateForceHandoffs checks every agent's force_handoff reference:
// the target must exist (or be an external reference), an agent cannot
// force-hand off to itself, and chains of force_handoff edges between
// local agents must not form a cycle — a cycle would make the run loop
// bounce between agents until max_iterations trips, which is never what
// the user intended.
func validateForceHandoffs(cfg *latest.Config, allNames map[string]bool) error {
	for _, agent := range cfg.Agents {
		ref := agent.ForceHandoff
		if ref == "" {
			continue
		}
		if ref == agent.Name {
			return fmt.Errorf("agent '%s' cannot force_handoff to itself", agent.Name)
		}
		if _, exists := allNames[ref]; !exists && !IsExternalReference(ref) {
			return fmt.Errorf("agent '%s' references non-existent force_handoff agent '%s'", agent.Name, ref)
		}
		if IsExternalReference(ref) {
			name, _ := ParseExternalAgentRef(ref)
			if allNames[name] {
				return fmt.Errorf("agent '%s': external force_handoff '%s' resolves to name '%s' which conflicts with a locally-defined agent", agent.Name, ref, name)
			}
		}
	}

	// Cycle detection: each agent has at most one outgoing force_handoff
	// edge, so walking the chain from every agent with a visited set is
	// linear overall. External references are leaves — they can't point
	// back into this config.
	edges := make(map[string]string, len(cfg.Agents))
	for _, agent := range cfg.Agents {
		if agent.ForceHandoff != "" && !IsExternalReference(agent.ForceHandoff) {
			edges[agent.Name] = agent.ForceHandoff
		}
	}
	for start := range edges {
		visited := map[string]bool{start: true}
		for cur, ok := edges[start], true; ok; cur, ok = edges[cur] {
			if visited[cur] {
				return fmt.Errorf("force_handoff cycle detected involving agent '%s'", cur)
			}
			visited[cur] = true
		}
	}

	return nil
}

// providerAPITypes are the allowed values for api_type in provider configs
var providerAPITypes = map[string]bool{
	"":                       true, // empty is allowed (defaults to openai_chatcompletions)
	"openai_chatcompletions": true,
	"openai_responses":       true,
}

// validateProviders validates all provider configurations
func validateProviders(cfg *latest.Config) error {
	if cfg.Providers == nil {
		return nil
	}

	for name, provCfg := range cfg.Providers {
		if err := ValidateProviderConfig(name, provCfg); err != nil {
			return err
		}
	}

	return nil
}

// ValidateProviderConfig validates a single named provider configuration, as
// found in the `providers` section of an agent file or in the user-level
// config.
func ValidateProviderConfig(name string, provCfg latest.ProviderConfig) error {
	// Validate provider name
	if err := validateProviderName(name); err != nil {
		return fmt.Errorf("provider '%s': %w", name, err)
	}

	// Validate api_type if set
	if !providerAPITypes[provCfg.APIType] {
		return fmt.Errorf("provider '%s': invalid api_type '%s' (must be one of: openai_chatcompletions, openai_responses)", name, provCfg.APIType)
	}

	// base_url is required for OpenAI-compatible providers (the default)
	// but optional for native providers like anthropic, google, amazon-bedrock
	if provCfg.BaseURL != "" {
		if _, err := url.Parse(provCfg.BaseURL); err != nil {
			return fmt.Errorf("provider '%s': invalid base_url '%s': %w", name, provCfg.BaseURL, err)
		}
	} else if isOpenAICustomProvider(provCfg) {
		return fmt.Errorf("provider '%s': base_url is required for OpenAI-compatible providers", name)
	}

	// token_key is optional - if not set, requests will be sent without bearer token

	return nil
}

// MergeGlobalProviders merges user-level provider definitions (from the user
// config) into an agent config's `providers` section. Definitions in the
// agent file win on name conflicts. Invalid global definitions are skipped
// with a warning so one bad user-config entry never breaks every run.
func MergeGlobalProviders(cfg *latest.Config, global map[string]latest.ProviderConfig) {
	for name, provCfg := range global {
		if _, exists := cfg.Providers[name]; exists {
			continue
		}
		if err := ValidateProviderConfig(name, provCfg); err != nil {
			slog.Warn("Ignoring invalid provider from user config", "provider", name, "error", err)
			continue
		}
		if cfg.Providers == nil {
			cfg.Providers = map[string]latest.ProviderConfig{}
		}
		cfg.Providers[name] = provCfg
	}
}

// isOpenAICustomProvider returns true if the provider config describes an OpenAI-compatible
// custom provider (i.e., Provider is empty or "openai", or api_type is explicitly set to an
// OpenAI schema). These providers require a base_url because they don't have a built-in default.
func isOpenAICustomProvider(cfg latest.ProviderConfig) bool {
	// If api_type is explicitly set, it's an OpenAI-compatible provider
	if cfg.APIType != "" {
		return true
	}
	// If provider is empty (defaults to openai) or explicitly "openai"
	return cfg.Provider == "" || cfg.Provider == "openai"
}

// validateProviderName validates that a provider name is valid
func validateProviderName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errors.New("name cannot be empty")
	}
	if trimmed != name {
		return errors.New("name cannot have leading or trailing whitespace")
	}
	if strings.Contains(name, "/") {
		return errors.New("name cannot contain '/'")
	}
	return nil
}

// validateSkillToolsetRefs checks that every toolset name referenced by an
// agent's inline fork skills resolves to a definition in the top-level
// `toolsets` section. Runs after resolveSkillDefinitions so skills merged in
// via use_skills are covered too.
func validateSkillToolsetRefs(cfg *latest.Config) error {
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		for j := range agent.Skills.Inline {
			inline := &agent.Skills.Inline[j]
			for _, ref := range inline.Toolsets {
				if _, ok := cfg.Toolsets[ref]; !ok {
					return fmt.Errorf("agent '%s' inline skill '%s' references non-existent toolset '%s'", agent.Name, inline.Name, ref)
				}
			}
		}
	}
	return nil
}

// validateSkills validates a skills configuration. label identifies the owner
// of the configuration in error messages (e.g. "agent 'foo'" or
// "skill group 'base'").
func validateSkills(label string, sc *latest.SkillsConfig) error {
	for _, source := range sc.Sources {
		switch {
		case source == latest.SkillSourceLocal:
			// valid
		case strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://"):
			if _, err := url.Parse(source); err != nil {
				return fmt.Errorf("%s has invalid skills source URL '%s': %w", label, source, err)
			}
		default:
			return fmt.Errorf("%s has unknown skills source '%s' (must be 'local' or an HTTP/HTTPS URL)", label, source)
		}
	}
	for _, name := range sc.Include {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%s has an empty skills entry", label)
		}
	}
	seenInline := make(map[string]bool, len(sc.Inline))
	for i := range sc.Inline {
		inline := &sc.Inline[i]
		if strings.TrimSpace(inline.Name) == "" {
			return fmt.Errorf("%s has an inline skill with no name", label)
		}
		// The name doubles as the `/<name>` slash command, whose parser splits
		// the input on the first space, so whitespace here would silently
		// produce a skill that command can never reach.
		if strings.ContainsFunc(inline.Name, unicode.IsSpace) {
			return fmt.Errorf("%s inline skill '%s' must not have whitespace in its name: it doubles as the /%s command", label, inline.Name, inline.Name)
		}
		if strings.TrimSpace(inline.Description) == "" {
			return fmt.Errorf("%s inline skill '%s' is missing a description", label, inline.Name)
		}
		if strings.TrimSpace(inline.Instructions) == "" {
			return fmt.Errorf("%s inline skill '%s' is missing instructions", label, inline.Name)
		}
		if inline.Context != "" && inline.Context != "fork" {
			return fmt.Errorf("%s inline skill '%s' has invalid context '%s' (only 'fork' is supported)", label, inline.Name, inline.Context)
		}
		if inline.Context != "fork" {
			if len(inline.Toolsets) > 0 {
				return fmt.Errorf("%s inline skill '%s' declares toolsets but is not a fork skill (set context: fork)", label, inline.Name)
			}
			if len(inline.AllowedTools) > 0 {
				return fmt.Errorf("%s inline skill '%s' declares allowed_tools but is not a fork skill (set context: fork)", label, inline.Name)
			}
		}
		if seenInline[inline.Name] {
			return fmt.Errorf("%s has duplicate inline skill '%s'", label, inline.Name)
		}
		seenInline[inline.Name] = true
	}
	return nil
}
