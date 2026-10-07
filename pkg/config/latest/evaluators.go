package latest

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"strings"
)

const (
	// EvaluatorProviderTypeSafe and EvaluatorProviderOpenAI are the evaluator backends.
	EvaluatorProviderTypeSafe = "typesafe"
	EvaluatorProviderOpenAI   = "openai"
)

// EvaluatorConfig defines a reusable assessment, independent of its consumers' policies.
//
// Provider may be omitted: Model is then a named entry of the top-level models
// map or an inline "provider/model" reference, as for chat models. With an
// explicit Provider, Model is the literal provider model ID.
type EvaluatorConfig struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	TokenKey string `json:"token_key,omitempty"`
	// BypassModelsGateway forces a direct provider connection, ignoring the
	// configured models gateway. A custom base_url or endpoint implies it.
	BypassModelsGateway bool              `json:"bypass_models_gateway,omitempty"`
	Type                string            `json:"type"`
	Instructions        string            `json:"instructions"`
	Choices             map[string]string `json:"choices,omitempty"`
	Levels              []string          `json:"levels,omitempty"`
	Timeout             Duration          `json:"timeout,omitzero"`
	Cost                *CostConfig       `json:"cost,omitempty"`
}

// Validate checks an evaluator definition before provider resolution.
func (e EvaluatorConfig) Validate() error {
	if e.Provider != "" && strings.TrimSpace(e.Provider) == "" {
		return errors.New("provider must not be blank")
	}
	if strings.TrimSpace(e.Model) == "" {
		return errors.New("model is required")
	}
	if strings.TrimSpace(e.Instructions) == "" {
		return errors.New("instructions are required")
	}
	if e.Timeout.Duration < 0 {
		return errors.New("timeout must not be negative")
	}
	if err := e.Cost.validate(); err != nil {
		return err
	}
	if e.Cost != nil {
		for _, price := range []float64{e.Cost.Input, e.Cost.Output, e.Cost.CacheRead, e.Cost.CacheWrite} {
			if math.IsNaN(price) || math.IsInf(price, 0) {
				return errors.New("cost prices must be finite")
			}
		}
	}
	for _, target := range []struct{ name, value string }{
		{"base_url", e.BaseURL},
		{"endpoint", e.Endpoint},
	} {
		// Environment references expand at team load; the result is checked again then.
		if target.value == "" || strings.Contains(target.value, "$") {
			continue
		}
		u, err := url.Parse(target.value)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return fmt.Errorf("%s must be an HTTP(S) URL without credentials, query, or fragment", target.name)
		}
	}
	switch e.Type {
	case "boolean":
		if len(e.Choices) != 0 || len(e.Levels) != 0 {
			return errors.New("boolean evaluators cannot define choices or levels")
		}
	case "choice":
		if len(e.Choices) < 2 || len(e.Choices) > 255 || len(e.Levels) != 0 {
			return errors.New("choice evaluators require 2-255 choices and no levels")
		}
		for choice := range e.Choices {
			if strings.TrimSpace(choice) == "" {
				return errors.New("choice names must not be empty")
			}
		}
	case "score":
		if len(e.Levels) < 2 || len(e.Levels) > 10 || len(e.Choices) != 0 {
			return errors.New("score evaluators require 2-10 levels and no choices")
		}
		for _, level := range e.Levels {
			if strings.TrimSpace(level) == "" {
				return errors.New("score levels must not be empty")
			}
		}
	default:
		return fmt.Errorf("unsupported evaluator type %q (expected boolean, choice, or score)", e.Type)
	}
	return nil
}

// Resolve applies connection defaults from a named provider without changing the definition.
// It cannot resolve named model references; use [EvaluatorConfig.ResolveWithModels].
func (e EvaluatorConfig) Resolve(providers map[string]ProviderConfig) (EvaluatorConfig, error) {
	return e.ResolveWithModels(providers, nil)
}

// ResolveWithModels resolves the model reference and provider defaults into a
// concrete backend, model ID and connection. Precedence: evaluator > referenced
// model > named provider > backend default. The definition is not modified, and
// environment references are left for [EvaluatorConfig.ExpandEnv].
func (e EvaluatorConfig) ResolveWithModels(providers map[string]ProviderConfig, models map[string]ModelConfig) (EvaluatorConfig, error) {
	if e.Provider == "" {
		var err error
		if e, err = e.resolveModelReference(models); err != nil {
			return e, err
		}
	}
	if p, ok := providers[e.Provider]; ok {
		underlying := cmp.Or(p.Provider, EvaluatorProviderOpenAI)
		if p.Auth != nil {
			return e, errors.New("evaluator providers do not support auth")
		}
		if p.APIType != "" {
			// Chat-only selectors never choose the Decisions endpoint, so a provider
			// shared with chat agents may keep them.
			chatOnly := p.APIType == "openai_responses" || p.APIType == "openai_chatcompletions"
			if underlying != EvaluatorProviderOpenAI || !chatOnly {
				return e, fmt.Errorf("evaluator providers do not support api_type %q", p.APIType)
			}
		}
		e.Provider = underlying
		e.BaseURL = cmp.Or(e.BaseURL, p.BaseURL)
		e.TokenKey = cmp.Or(e.TokenKey, p.TokenKey)
	}
	switch e.Provider {
	case EvaluatorProviderTypeSafe:
		e.TokenKey = cmp.Or(e.TokenKey, "TYPESAFE_API_KEY")
	case EvaluatorProviderOpenAI:
		e.TokenKey = cmp.Or(e.TokenKey, "OPENAI_API_KEY")
	default:
		return e, fmt.Errorf("unsupported evaluator provider %q", e.Provider)
	}
	return e, e.Validate()
}

// resolveModelReference replaces a provider-less Model with a provider and model ID.
func (e EvaluatorConfig) resolveModelReference(models map[string]ModelConfig) (EvaluatorConfig, error) {
	if named, ok := models[e.Model]; ok {
		if err := named.evaluatorCompatible(); err != nil {
			return e, fmt.Errorf("model %q: %w", e.Model, err)
		}
		if strings.TrimSpace(named.Provider) == "" || strings.TrimSpace(named.Model) == "" {
			return e, fmt.Errorf("model %q must define provider and model", e.Model)
		}
		e.Provider, e.Model = named.Provider, named.Model
		e.BaseURL = cmp.Or(e.BaseURL, named.BaseURL)
		e.TokenKey = cmp.Or(e.TokenKey, named.TokenKey)
		e.BypassModelsGateway = e.BypassModelsGateway || named.BypassModelsGateway
		return e, nil
	}
	ref, err := ParseModelRef(e.Model)
	if err != nil || strings.TrimSpace(ref.Provider) == "" || strings.TrimSpace(ref.Model) == "" {
		return e, fmt.Errorf("unknown model reference %q: expected a name from models or 'provider/model', or set provider", e.Model)
	}
	e.Provider, e.Model = ref.Provider, ref.Model
	return e, nil
}

// evaluatorAllowedModelFields lists the ModelConfig fields a named model may set
// when an evaluator references it: identity, connection and descriptive
// metadata. Pricing and capabilities are chat settings that are not inherited.
var evaluatorAllowedModelFields = map[string]bool{
	"Name": true, "Provider": true, "Model": true, "Description": true, "DisplayModel": true,
	"BaseURL": true, "TokenKey": true, "BypassModelsGateway": true,
	"Cost": true, "Capabilities": true, "OutputCapabilities": true,
}

// evaluatorCompatible rejects model settings an evaluator cannot honor rather than ignoring them.
func (m ModelConfig) evaluatorCompatible() error {
	v := reflect.ValueOf(m)
	for i := range v.NumField() {
		field := v.Type().Field(i)
		if evaluatorAllowedModelFields[field.Name] || v.Field(i).IsZero() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		return fmt.Errorf("%s is not supported by evaluators; use a connection-only model entry or an inline provider/model reference", name)
	}
	return nil
}

// ExpandedEnv returns a copy with environment references substituted in the
// value-bearing connection fields, like [ModelConfig.ExpandEnv]. Instructions
// and criteria are untouched.
func (e EvaluatorConfig) ExpandedEnv(expand func(string) (string, error)) (EvaluatorConfig, error) {
	if expand == nil {
		return e, nil
	}
	for _, field := range []*string{&e.Model, &e.BaseURL, &e.Endpoint} {
		if *field == "" {
			continue
		}
		expanded, err := expand(*field)
		if err != nil {
			return e, err
		}
		*field = expanded
	}
	return e, nil
}

// UsesModelsGateway reports whether a resolved evaluator connects through gateway
// rather than directly: custom URLs and an explicit bypass always dial the provider.
// It is the single connection-mode rule shared by the runtime and credential discovery.
func (e EvaluatorConfig) UsesModelsGateway(gateway string) bool {
	return gateway != "" && !e.BypassModelsGateway && e.BaseURL == "" && e.Endpoint == ""
}

// EvaluatorPolicy maps categorical assessments to tool-guard decisions.
// Boolean assessments use the keys "true" and "false".
type EvaluatorPolicy struct {
	Decisions      map[string]string `json:"decisions"`
	MinProbability float64           `json:"min_probability"`
	Fallback       string            `json:"fallback"`
}

// Validate checks a policy independently of its referenced evaluator.
func (p *EvaluatorPolicy) Validate() error {
	if p == nil {
		return errors.New("evaluator_policy is required")
	}
	if len(p.Decisions) == 0 {
		return errors.New("evaluator_policy.decisions must not be empty")
	}
	if math.IsNaN(p.MinProbability) || math.IsInf(p.MinProbability, 0) || p.MinProbability <= 0 || p.MinProbability > 1 {
		return errors.New("evaluator_policy.min_probability must be greater than 0 and at most 1")
	}
	if p.Fallback != "ask" && p.Fallback != "deny" {
		return errors.New("evaluator_policy.fallback must be ask or deny")
	}
	for _, decision := range p.Decisions {
		if decision != "allow" && decision != "ask" && decision != "deny" {
			return errors.New("evaluator_policy decisions must be allow, ask, or deny")
		}
	}
	return nil
}

func (p *EvaluatorPolicy) validateEvaluator(e EvaluatorConfig) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if e.Type == "score" {
		return errors.New("tool guards require a boolean or choice evaluator; score assessments are available through the Go API")
	}
	for key := range p.Decisions {
		if e.Type == "boolean" {
			if key != "true" && key != "false" {
				return fmt.Errorf("unknown boolean outcome %q (expected true or false)", key)
			}
		} else if _, ok := e.Choices[key]; !ok {
			return fmt.Errorf("unknown evaluator choice %q", key)
		}
	}
	return nil
}

// ValidateEvaluators validates definitions and hook references. Provider aliases
// resolve at team load, after user-level providers have been merged.
func (t *Config) ValidateEvaluators() error {
	for name, def := range t.Evaluators {
		if strings.TrimSpace(name) == "" {
			return errors.New("evaluator names must not be empty")
		}
		if err := def.Validate(); err != nil {
			return fmt.Errorf("evaluators.%s: %w", name, err)
		}
	}
	for _, a := range t.Agents {
		for event, matchers := range a.Hooks.Events() {
			for _, matcher := range matchers {
				for _, hook := range matcher.Hooks {
					if hook.Type != "evaluator" {
						continue
					}
					def, ok := t.Evaluators[hook.Evaluator]
					if !ok {
						return fmt.Errorf("agents.%s.hooks.%s: unknown evaluator %q", a.Name, event, hook.Evaluator)
					}
					if hook.RoutingPolicy != nil {
						continue // validated with the agent's routing declaration
					}
					if err := hook.EvaluatorPolicy.validateEvaluator(def); err != nil {
						return fmt.Errorf("agents.%s.hooks.%s: %w", a.Name, event, err)
					}
				}
			}
		}
	}
	return t.validateRouting()
}
