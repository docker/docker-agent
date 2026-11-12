package latest

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
)

// EvaluatorConfig defines a reusable assessment, independent of its consumers' policies.
type EvaluatorConfig struct {
	Provider            string            `json:"provider"`
	Model               string            `json:"model"`
	BaseURL             string            `json:"base_url,omitempty"`
	Endpoint            string            `json:"endpoint,omitempty"`
	TokenKey            string            `json:"token_key,omitempty"`
	Type                string            `json:"type"`
	Instructions        string            `json:"instructions"`
	Choices             map[string]string `json:"choices,omitempty"`
	Levels              []string          `json:"levels,omitempty"`
	Timeout             Duration          `json:"timeout,omitzero"`
	Cost                *CostConfig       `json:"cost,omitempty"`
	BypassModelsGateway bool              `json:"bypass_models_gateway,omitempty"`
}

// Validate checks an evaluator definition before provider resolution.
func (e EvaluatorConfig) Validate() error {
	if strings.TrimSpace(e.Provider) == "" || strings.TrimSpace(e.Model) == "" {
		return errors.New("provider and model are required")
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
		if target.value == "" {
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
func (e EvaluatorConfig) Resolve(providers map[string]ProviderConfig) (EvaluatorConfig, error) {
	if p, ok := providers[e.Provider]; ok {
		if p.Auth != nil || p.APIType != "" {
			return e, errors.New("evaluator providers do not support auth or api_type")
		}
		e.Provider = p.Provider
		e.BaseURL = cmp.Or(e.BaseURL, p.BaseURL)
		e.TokenKey = cmp.Or(e.TokenKey, p.TokenKey)
	}
	switch e.Provider {
	case "typesafe":
		e.TokenKey = cmp.Or(e.TokenKey, "TYPESAFE_API_KEY")
	case "openai":
		e.TokenKey = cmp.Or(e.TokenKey, "OPENAI_API_KEY")
	default:
		return e, fmt.Errorf("unsupported evaluator provider %q", e.Provider)
	}
	return e, e.Validate()
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
