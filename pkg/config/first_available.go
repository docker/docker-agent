package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/docker/docker-agent/pkg/cli/invocation"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	modelprovider "github.com/docker/docker-agent/pkg/model/provider"
)

// ResolveFirstAvailableModels resolves reachable `first_available` models in the
// config into concrete provider/model definitions by selecting the first
// candidate whose credentials are configured. The config is mutated in place so
// the rest of the pipeline (env-var gathering, model instantiation, runtime
// switching) sees regular model definitions for models that can be reached from
// agents, fallbacks, tool model overrides, skills, RAG reranking, or routing.
//
// Candidates are tried in order. A candidate is considered available when all
// the environment variables it requires are set; local providers (dmr, ollama)
// require none and therefore make reliable final fallbacks. When a models
// gateway is configured, the first candidate is selected since the gateway
// supplies the credentials.
func ResolveFirstAvailableModels(ctx context.Context, cfg *latest.Config, modelsGateway string, env environment.Provider) error {
	// Snapshot which models are selectors before mutating the map, so the
	// nested-selector check is independent of map iteration order.
	selectors := map[string]bool{}
	for name, m := range cfg.Models {
		if m.IsFirstAvailable() {
			selectors[name] = true
		}
	}
	if len(selectors) == 0 {
		return nil
	}

	reachable := reachableFirstAvailableModels(cfg, selectors)
	for _, name := range sortedKeys(reachable) {
		if err := resolveFirstAvailableModel(ctx, cfg, name, selectors, modelsGateway, env); err != nil {
			return err
		}
	}

	return nil
}

func resolveFirstAvailableModel(ctx context.Context, cfg *latest.Config, name string, selectors map[string]bool, modelsGateway string, env environment.Provider) error {
	m := cfg.Models[name]
	chosen, ref, err := selectFirstAvailable(ctx, cfg, m.FirstAvailable, selectors, modelsGateway, env)
	if err != nil {
		var missingErr *firstAvailableMissingEnvError
		if errors.As(err, &missingErr) {
			return missingErr
		}
		return fmt.Errorf("model '%s': %w", name, err)
	}

	slog.DebugContext(ctx, "Resolved first_available model",
		"model_name", name, "selected", ref, "provider", chosen.Provider, "model", chosen.Model)
	// The selector's description documents the selection itself; keep it over
	// the chosen candidate's own description.
	if m.Description != "" {
		chosen.Description = m.Description
	}
	cfg.Models[name] = chosen
	return nil
}

func reachableFirstAvailableModels(cfg *latest.Config, selectors map[string]bool) map[string]bool {
	reachable := map[string]bool{}
	seen := map[string]bool{}

	hasRootReference := false

	var visit func(string)
	visit = func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			return
		}
		hasRootReference = true
		seen[ref] = true

		model, exists := cfg.Models[ref]
		if !exists {
			return
		}
		if selectors[ref] {
			reachable[ref] = true
			return
		}
		for _, rule := range model.Routing {
			visit(rule.Model)
		}
	}

	for _, agent := range cfg.Agents {
		if agent.Harness == nil {
			for ref := range strings.SplitSeq(agent.Model, ",") {
				visit(ref)
			}
			for _, ref := range agent.GetFallbackModels() {
				visit(ref)
			}
			// The effective compaction model (agent > model > provider
			// precedence) may name a selector too.
			visit(EffectiveCompactionModelRef(cfg, &agent))
		}

		for _, toolset := range agent.Toolsets {
			visit(toolset.Model)
			if toolset.RAGConfig != nil && toolset.RAGConfig.Results.Reranking != nil {
				visit(toolset.RAGConfig.Results.Reranking.Model)
			}
		}

		for _, skill := range agent.Skills.Inline {
			if skill.Context == "fork" {
				visit(skill.Model)
			}
		}
	}

	if !hasRootReference {
		return selectors
	}

	return reachable
}

// selectFirstAvailable returns the config of the first candidate that has
// credentials configured, along with the reference that was selected.
func selectFirstAvailable(ctx context.Context, cfg *latest.Config, refs []string, selectors map[string]bool, modelsGateway string, env environment.Provider) (latest.ModelConfig, string, error) {
	var missingByCandidate []firstAvailableMissingCandidate

	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}

		candidate, err := resolveCandidate(cfg, ref, selectors)
		if err != nil {
			return latest.ModelConfig{}, "", err
		}

		missing, err := modelMissingCredentials(ctx, cfg, &candidate, modelsGateway, env)
		if err != nil {
			return latest.ModelConfig{}, "", err
		}
		if len(missing) == 0 {
			return candidate, ref, nil
		}
		missingByCandidate = append(missingByCandidate, firstAvailableMissingCandidate{
			Ref:     ref,
			Missing: missing,
		})
	}

	if len(missingByCandidate) > 0 {
		return latest.ModelConfig{}, "", &firstAvailableMissingEnvError{Candidates: missingByCandidate}
	}
	return latest.ModelConfig{}, "", fmt.Errorf("no first_available candidate has credentials configured (tried: %s)", strings.Join(refs, ", "))
}

type firstAvailableMissingCandidate struct {
	Ref     string
	Missing []string
}

type firstAvailableMissingEnvError struct {
	Candidates []firstAvailableMissingCandidate
}

var _ error = &firstAvailableMissingEnvError{}

func (e *firstAvailableMissingEnvError) Error() string {
	var msg strings.Builder

	fmt.Fprintln(&msg, "No 'first_available' candidate has credentials configured.")
	fmt.Fprintln(&msg, "Set the environment variables for at least one candidate:")
	example := "OPENAI_API_KEY"
	for i, candidate := range e.Candidates {
		fmt.Fprintf(&msg, " - %s: %s\n", candidate.Ref, strings.Join(candidate.Missing, ", "))
		if i == 0 && len(candidate.Missing) > 0 {
			example = candidate.Missing[0]
		}
	}
	msg.WriteString("\n")
	msg.WriteString(environment.SecretSourcesHelp(example))
	fmt.Fprintf(&msg, "\nOr run `%s setup` to configure a provider or local model interactively.\n", invocation.DockerAgent())
	fmt.Fprintf(&msg, "Step-by-step model setup (API key or local): %s\n", environment.ModelSetupDocsURL)

	return msg.String()
}

// MissingModelCredentials reports that the missing variables are model
// credentials: first_available candidates are models by definition. It lets
// callers (e.g. the CLI's setup offer) classify this error without exporting
// the type.
func (e *firstAvailableMissingEnvError) MissingModelCredentials() bool { return true }

// resolveCandidate turns a candidate reference into a ModelConfig. The
// reference is first looked up as a named model; otherwise it is parsed as an
// inline "provider/model" spec. A candidate that is itself a first_available
// selector is rejected to avoid recursive selectors.
func resolveCandidate(cfg *latest.Config, ref string, selectors map[string]bool) (latest.ModelConfig, error) {
	if selectors[ref] {
		return latest.ModelConfig{}, fmt.Errorf("first_available candidate '%s' is itself a first_available selector", ref)
	}
	if mc, exists := cfg.Models[ref]; exists {
		if err := validateCandidateProvider(cfg, ref, mc.Provider); err != nil {
			return latest.ModelConfig{}, err
		}
		return mc, nil
	}

	parsed, err := latest.ParseModelRef(ref)
	if err != nil {
		return latest.ModelConfig{}, fmt.Errorf("first_available candidate '%s' is not a known model nor a valid 'provider/model' spec", ref)
	}
	if err := validateCandidateProvider(cfg, ref, parsed.Provider); err != nil {
		return latest.ModelConfig{}, err
	}
	return parsed, nil
}

func validateCandidateProvider(cfg *latest.Config, ref, provider string) error {
	if provider == "" || modelprovider.IsKnownProvider(provider) {
		return nil
	}
	if _, exists := cfg.Providers[provider]; exists {
		return nil
	}
	return fmt.Errorf("first_available candidate '%s' uses unknown provider '%s'", ref, provider)
}

// modelMissingCredentials returns the credentials required by a model that are
// not configured in the environment. Models that require no env vars (e.g.
// local dmr/ollama providers) have no missing credentials and are considered
// available. When a gateway is configured, credentials are supplied by the
// gateway — except for candidates that bypass it (explicit
// bypass_models_gateway or a custom base_url, which implies the bypass) and
// therefore still need their own credentials.
func modelMissingCredentials(ctx context.Context, cfg *latest.Config, model *latest.ModelConfig, modelsGateway string, env environment.Provider) ([]string, error) {
	if modelsGateway != "" && !model.BypassModelsGateway && !latest.HasCustomBaseURL(*model, cfg.Providers) {
		return nil, nil
	}

	required, err := requiredEnvVarsForModelConfig(ctx, model, cfg, env)
	if err != nil {
		return nil, err
	}

	var missing []string
	for _, name := range sortedKeys(required) {
		if v, _ := env.Get(ctx, name); v == "" {
			slog.DebugContext(ctx, "First-available candidate missing credentials", "env", name, "provider", model.Provider, "model", model.Model)
			missing = append(missing, name)
		}
	}
	return missing, nil
}

func requiredEnvVarsForModelConfig(ctx context.Context, model *latest.ModelConfig, cfg *latest.Config, env environment.Provider) (map[string]bool, error) {
	required := map[string]bool{}
	addEnvVarsForModelConfig(ctx, model, cfg.Providers, required, env)

	for _, rule := range model.Routing {
		ruleModel, err := resolveRoutingRuleModel(cfg, rule.Model)
		if err != nil {
			return nil, err
		}
		addEnvVarsForModelConfig(ctx, &ruleModel, cfg.Providers, required, env)
	}

	return required, nil
}

func resolveRoutingRuleModel(cfg *latest.Config, ref string) (latest.ModelConfig, error) {
	if model, exists := cfg.Models[ref]; exists {
		if model.IsFirstAvailable() {
			return latest.ModelConfig{}, fmt.Errorf("routing target '%s' is a first_available selector and cannot be used as a first_available candidate dependency", ref)
		}
		if len(model.Routing) > 0 {
			return latest.ModelConfig{}, fmt.Errorf("routing target '%s' has routing rules and cannot be used as a first_available candidate dependency", ref)
		}
		if err := validateCandidateProvider(cfg, ref, model.Provider); err != nil {
			return latest.ModelConfig{}, err
		}
		return model, nil
	}

	parsed, err := latest.ParseModelRef(ref)
	if err != nil {
		return latest.ModelConfig{}, fmt.Errorf("routing target '%s' is not a known model nor a valid 'provider/model' spec", ref)
	}
	if err := validateCandidateProvider(cfg, ref, parsed.Provider); err != nil {
		return latest.ModelConfig{}, err
	}
	return parsed, nil
}
