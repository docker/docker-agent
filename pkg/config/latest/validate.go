package latest

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

func (t *Config) UnmarshalYAML(unmarshal func(any) error) error {
	type alias Config
	var tmp alias
	if err := unmarshal(&tmp); err != nil {
		return err
	}
	*t = Config(tmp)
	return t.Validate()
}

func (t *Config) Validate() error {
	if err := t.Budget.validate(); err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	if t.Runtime != nil {
		if err := t.Runtime.Safety.Validate(); err != nil {
			return fmt.Errorf("runtime.safety: %w", err)
		}
	}
	for name := range t.Budgets {
		b := t.Budgets[name]
		if err := b.validate(); err != nil {
			return fmt.Errorf("budgets.%s: %w", name, err)
		}
	}
	for name, p := range t.Providers {
		if err := p.Auth.Validate(p.Provider); err != nil {
			return fmt.Errorf("providers.%s: %w", name, err)
		}
	}
	for name, m := range t.Models {
		if err := m.validateFirstAvailable(); err != nil {
			return fmt.Errorf("models.%s: %w", name, err)
		}
		if err := validateCompactionThreshold(m.CompactionThreshold); err != nil {
			return fmt.Errorf("models.%s: %w", name, err)
		}
		if err := m.Cost.validate(); err != nil {
			return fmt.Errorf("models.%s: %w", name, err)
		}
		if err := m.Auth.Validate(EffectiveProviderType(m, t.Providers)); err != nil {
			return fmt.Errorf("models.%s: %w", name, err)
		}
	}

	// Re-validate reusable, named toolset definitions here so they are
	// checked even when no agent references them, and when a Config is
	// constructed programmatically rather than parsed from YAML. (When
	// parsed, each value is already validated by Toolset.UnmarshalYAML.)
	// Mirrors the agent-toolset validation loop below.
	for name := range t.Toolsets {
		ts := t.Toolsets[name]
		if err := ts.validate(); err != nil {
			return fmt.Errorf("toolsets.%s: %w", name, err)
		}
	}

	// Top-level "rag" definitions skip Toolset.validate() during YAML
	// unmarshaling (see RAGToolset.UnmarshalYAML), so their RAGConfig is
	// validated here instead.
	for name, def := range t.RAG {
		if def.RAGConfig == nil {
			continue
		}
		if err := def.RAGConfig.validate(); err != nil {
			return fmt.Errorf("rag.%s: %w", name, err)
		}
	}

	for i := range t.Agents {
		agent := &t.Agents[i]

		// Validate fallback config
		if err := agent.validateFallback(); err != nil {
			return err
		}
		if err := agent.validateHarness(); err != nil {
			return err
		}
		if err := agent.Safety.Validate(); err != nil {
			return fmt.Errorf("agents.%s.safety: %w", agent.Name, err)
		}
		if err := agent.StructuredOutput.validate(); err != nil {
			return fmt.Errorf("agents.%s.structured_output: %w", agent.Name, err)
		}
		if err := validateCompactionThreshold(agent.CompactionThreshold); err != nil {
			return fmt.Errorf("agents.%s: %w", agent.Name, err)
		}
		if agent.AddPromptFilesDepth < 0 {
			return fmt.Errorf("agents.%s: add_prompt_files_depth must be >= 0, got %d", agent.Name, agent.AddPromptFilesDepth)
		}
		for _, name := range agent.Budgets {
			if _, ok := t.Budgets[name]; !ok {
				return fmt.Errorf("agents.%s: budgets: unknown budget %q; define it under the top-level 'budgets'", agent.Name, name)
			}
		}

		for j := range agent.Toolsets {
			if err := agent.Toolsets[j].validate(); err != nil {
				return err
			}
		}
		if agent.Hooks != nil {
			if err := agent.Hooks.Validate(); err != nil {
				return err
			}
		}
	}

	return nil
}

// validateFirstAvailable validates a model's first_available selector. The
// selector is mutually exclusive with other model configuration fields, and
// every candidate reference must be non-empty.
func (m *ModelConfig) validateFirstAvailable() error {
	if m.FirstAvailable == nil {
		return nil
	}
	if len(m.FirstAvailable) == 0 {
		return errors.New("first_available must contain at least one candidate")
	}
	if m.Provider != "" || m.Model != "" {
		return errors.New("first_available cannot be combined with provider/model")
	}
	if m.Temperature != nil {
		return errors.New("first_available cannot be combined with temperature")
	}
	if m.MaxTokens != nil {
		return errors.New("first_available cannot be combined with max_tokens")
	}
	if m.TopP != nil {
		return errors.New("first_available cannot be combined with top_p")
	}
	if m.FrequencyPenalty != nil {
		return errors.New("first_available cannot be combined with frequency_penalty")
	}
	if m.PresencePenalty != nil {
		return errors.New("first_available cannot be combined with presence_penalty")
	}
	if m.BaseURL != "" {
		return errors.New("first_available cannot be combined with base_url")
	}
	if m.TokenKey != "" {
		return errors.New("first_available cannot be combined with token_key")
	}
	if m.BypassModelsGateway {
		return errors.New("first_available cannot be combined with bypass_models_gateway (set it on the candidate models instead)")
	}
	if len(m.ProviderOpts) > 0 {
		return errors.New("first_available cannot be combined with provider_opts")
	}
	if m.TrackUsage != nil {
		return errors.New("first_available cannot be combined with track_usage")
	}
	if m.ThinkingBudget != nil {
		return errors.New("first_available cannot be combined with thinking_budget")
	}
	if m.TaskBudget != nil {
		return errors.New("first_available cannot be combined with task_budget")
	}
	if m.Auth != nil {
		return errors.New("first_available cannot be combined with auth")
	}
	if len(m.Routing) > 0 {
		return errors.New("first_available cannot be combined with routing")
	}
	if m.TitleModel != "" {
		return errors.New("first_available cannot be combined with title_model")
	}
	if m.CompactionModel != "" {
		return errors.New("first_available cannot be combined with compaction_model")
	}
	if m.CompactionThreshold != nil {
		return errors.New("first_available cannot be combined with compaction_threshold")
	}
	if m.Cost != nil {
		return errors.New("first_available cannot be combined with cost (set it on the candidate models instead)")
	}
	if m.OutputCapabilities != nil {
		return errors.New("first_available cannot be combined with output_capabilities (set output_capabilities.image on the candidate models instead)")
	}
	for i, ref := range m.FirstAvailable {
		if strings.TrimSpace(ref) == "" {
			return fmt.Errorf("first_available[%d] must not be empty", i)
		}
	}
	return nil
}

// validateCompactionThreshold rejects a compaction_threshold outside (0, 1].
// nil (unset) is valid and means "use the default".
func validateCompactionThreshold(v *float64) error {
	if v == nil {
		return nil
	}
	if *v <= 0 || *v > 1 {
		return fmt.Errorf("compaction_threshold must be greater than 0 and at most 1, got %v", *v)
	}
	return nil
}

// validateFallback validates the fallback configuration for an agent
func (a *AgentConfig) validateFallback() error {
	if a.Fallback == nil {
		return nil
	}

	// -1 is allowed as a special value meaning "explicitly no retries"
	if a.Fallback.Retries < -1 {
		return errors.New("fallback.retries must be >= -1 (use -1 for no retries, 0 for default)")
	}
	if a.Fallback.Cooldown.Duration < 0 {
		return errors.New("fallback.cooldown must be non-negative")
	}

	return nil
}

func (a *AgentConfig) validateHarness() error {
	if a.Harness == nil {
		return nil
	}

	// Harness agents skip docker-agent's model and compaction resolution: the
	// harness manages its own context. Reject a setting that would silently be
	// ignored.
	if a.CompactionModel != "" {
		return errors.New("compaction_model cannot be used with a harness; the harness manages its own context compaction")
	}

	// Harness agents also skip docker-agent's run loop, so the internal tool
	// behind tool-mode structured output is never injected or enforced.
	// Native mode stays accepted: the harness may support it itself.
	if a.StructuredOutput.ToolMode() {
		return errors.New("structured_output.mode 'tool' cannot be used with a harness; the harness does not run the tool loop that enforces it (use mode 'native')")
	}

	h := a.Harness
	switch h.Type {
	case "claude-code", "codex", "pi", "opencode":
	case "":
		return errors.New("harness.type is required")
	default:
		return fmt.Errorf("unsupported harness.type %q (must be one of: claude-code, codex, pi, opencode)", h.Type)
	}

	if h.Effort != "" {
		if h.Type != "claude-code" {
			return errors.New("harness.effort can only be used with harness.type 'claude-code'")
		}
		switch h.Effort {
		case "low", "medium", "high", "xhigh", "max":
		default:
			return errors.New("harness.effort must be one of: low, medium, high, xhigh, max")
		}
	}
	if h.Agent != "" && h.Type != "opencode" {
		return errors.New("harness.agent can only be used with harness.type 'opencode'")
	}
	if h.Thinking && h.Type != "opencode" {
		return errors.New("harness.thinking can only be used with harness.type 'opencode'")
	}

	return nil
}

func (t *Toolset) validate() error {
	// Attributes used on the wrong toolset type.
	if len(t.Shell) > 0 && t.Type != "script" {
		return errors.New("shell can only be used with type 'script'")
	}
	if t.Path != "" && t.Type != "memory" && t.Type != "tasks" {
		return errors.New("path can only be used with type 'memory' or 'tasks'")
	}
	if len(t.PostEdit) > 0 && t.Type != "filesystem" && t.Type != "file" {
		return errors.New("post_edit can only be used with type 'filesystem' or 'file'")
	}
	if t.IgnoreVCS != nil && t.Type != "filesystem" {
		return errors.New("ignore_vcs can only be used with type 'filesystem'")
	}
	if len(t.AllowList) > 0 && t.Type != "filesystem" && t.Type != "file" {
		return errors.New("allow_list can only be used with type 'filesystem' or 'file'")
	}
	if len(t.DenyList) > 0 && t.Type != "filesystem" && t.Type != "file" {
		return errors.New("deny_list can only be used with type 'filesystem' or 'file'")
	}
	if err := validatePathRootEntries("allow_list", t.AllowList); err != nil {
		return err
	}
	if err := validatePathRootEntries("deny_list", t.DenyList); err != nil {
		return err
	}
	if len(t.Env) > 0 && (t.Type != "shell" && t.Type != "background_jobs" && t.Type != "script" && t.Type != "mcp" && t.Type != "lsp") {
		return errors.New("env can only be used with type 'shell', 'background_jobs', 'script', 'mcp' or 'lsp'")
	}
	if len(t.FileTypes) > 0 && t.Type != "lsp" {
		return errors.New("file_types can only be used with type 'lsp'")
	}
	if len(t.AllowedServers) > 0 && t.Type != "mcp_catalog" {
		return errors.New("allowed_servers can only be used with type 'mcp_catalog'")
	}
	if len(t.BlockedServers) > 0 && t.Type != "mcp_catalog" {
		return errors.New("blocked_servers can only be used with type 'mcp_catalog'")
	}
	if err := validateNonEmptyEntries("allowed_servers", t.AllowedServers); err != nil {
		return err
	}
	if err := validateNonEmptyEntries("blocked_servers", t.BlockedServers); err != nil {
		return err
	}
	if len(t.AllowedDomains) > 0 && t.Type != "fetch" {
		return errors.New("allowed_domains can only be used with type 'fetch'")
	}
	if len(t.BlockedDomains) > 0 && t.Type != "fetch" {
		return errors.New("blocked_domains can only be used with type 'fetch'")
	}
	if t.AllowPrivateIPsEnabled() && t.Type != "fetch" && t.Type != "mcp" && t.Type != "api" && t.Type != "openapi" && t.Type != "a2a" {
		return errors.New("allow_private_ips can only be used with type 'fetch', 'api', 'openapi', 'a2a' or remote MCP toolsets")
	}
	if t.SudoAskpass != nil && t.Type != "shell" {
		return errors.New("sudo_askpass can only be used with type 'shell'")
	}
	if t.Recall != nil && t.Type != "background_jobs" {
		return errors.New("recall can only be used with type 'background_jobs'")
	}
	if len(t.AllowedDomains) > 0 && len(t.BlockedDomains) > 0 {
		return errors.New("allowed_domains and blocked_domains are mutually exclusive")
	}
	if err := validateDomainPatterns("allowed_domains", t.AllowedDomains); err != nil {
		return err
	}
	if err := validateDomainPatterns("blocked_domains", t.BlockedDomains); err != nil {
		return err
	}
	if len(t.Models) > 0 && t.Type != "model_picker" {
		return errors.New("models can only be used with type 'model_picker'")
	}
	if t.Shared && t.Type != "todo" {
		return errors.New("shared can only be used with type 'todo'")
	}
	if t.Version != "" && t.Type != "mcp" && t.Type != "lsp" {
		return errors.New("version can only be used with type 'mcp' or 'lsp'")
	}
	if t.Command != "" && t.Type != "mcp" && t.Type != "lsp" {
		return errors.New("command can only be used with type 'mcp' or 'lsp'")
	}
	if len(t.Args) > 0 && t.Type != "mcp" && t.Type != "lsp" {
		return errors.New("args can only be used with type 'mcp' or 'lsp'")
	}
	if t.Ref != "" && t.Type != "mcp" && t.Type != "rag" {
		return errors.New("ref can only be used with type 'mcp' or 'rag'")
	}
	if (t.Remote.URL != "" || t.Remote.TransportType != "" || t.Remote.OAuth != nil) && t.Type != "mcp" {
		return errors.New("remote can only be used with type 'mcp'")
	}
	if (len(t.Remote.Headers) > 0) && (t.Type != "mcp" && t.Type != "a2a") {
		return errors.New("remote headers can only be used with type 'mcp' or 'a2a'")
	}
	if len(t.Headers) > 0 && t.Type != "openapi" && t.Type != "a2a" && t.Type != "fetch" {
		return errors.New("headers can only be used with type 'openapi', 'a2a' or 'fetch'")
	}
	if t.Config != nil && t.Type != "mcp" {
		return errors.New("config can only be used with type 'mcp'")
	}
	if t.URL != "" && t.Type != "a2a" && t.Type != "openapi" && t.Type != "open_url" {
		return errors.New("url can only be used with type 'a2a', 'openapi' or 'open_url'")
	}
	if t.Name != "" && (t.Type != "mcp" && t.Type != "a2a" && t.Type != "rag" && t.Type != "open_url") {
		return errors.New("name can only be used with type 'mcp', 'a2a', 'rag', or 'open_url'")
	}
	if t.RAGConfig != nil && t.Type != "rag" {
		return errors.New("rag_config can only be used with type 'rag'")
	}
	if t.WorkingDir != "" && t.Type != "mcp" && t.Type != "lsp" {
		return errors.New("working_dir can only be used with type 'mcp' or 'lsp'")
	}
	// working_dir requires a local subprocess; it is meaningless for remote MCP toolsets.
	if t.WorkingDir != "" && t.Type == "mcp" && t.Remote.URL != "" {
		return errors.New("working_dir is not valid for remote MCP toolsets (no local subprocess)")
	}
	if t.Lifecycle != nil && t.Type != "mcp" && t.Type != "lsp" {
		return errors.New("lifecycle can only be used with type 'mcp' or 'lsp'")
	}
	if err := t.Lifecycle.validate(); err != nil {
		return err
	}

	switch t.Type {
	case "shell":
		// no additional validation needed
	case "background_jobs":
		// no additional validation needed
	case "memory":
		// path is optional; defaults to ~/.cagent/memory/<agent-name>/memory.db
	case "tasks":
		// path defaults to ./tasks.json if not set
	case "mcp":
		count := 0
		if t.Command != "" {
			count++
		}
		if t.Remote.URL != "" {
			count++
		}
		if t.Ref != "" {
			count++
		}
		if count == 0 {
			return errors.New("either command, remote or ref must be set")
		}
		if count > 1 {
			return errors.New("either command, remote or ref must be set, but only one of those")
		}
		if t.AllowPrivateIPsEnabled() && t.Remote.URL == "" && t.Ref == "" {
			return errors.New("allow_private_ips can only be used with type 'fetch', 'api', 'openapi', 'a2a' or remote MCP toolsets")
		}
		if t.Remote.OAuth != nil {
			if t.Remote.URL == "" {
				return errors.New("oauth requires remote url to be set")
			}
			if t.Remote.OAuth.ClientID == "" && t.Remote.OAuth.ClientSecret != "" {
				return errors.New("oauth clientSecret requires clientId to be set")
			}
			if t.Remote.OAuth.CallbackPort != 0 && (t.Remote.OAuth.CallbackPort < 1 || t.Remote.OAuth.CallbackPort > 65535) {
				return errors.New("oauth callbackPort must be between 1 and 65535")
			}
			if t.Remote.OAuth.CallbackRedirectURL != "" {
				if err := validateCallbackRedirectURL(t.Remote.OAuth.CallbackRedirectURL); err != nil {
					return err
				}
			}
		}
	case "a2a":
		if t.URL == "" {
			return errors.New("a2a toolset requires a url to be set")
		}
	case "lsp":
		if t.Command == "" {
			return errors.New("lsp toolset requires a command to be set")
		}
	case "openapi":
		if t.URL == "" {
			return errors.New("openapi toolset requires a url to be set")
		}
	case "open_url":
		if t.URL == "" {
			return errors.New("open_url toolset requires a url to be set")
		}
	case "model_picker":
		if len(t.Models) == 0 {
			return errors.New("model_picker toolset requires at least one model in the 'models' list")
		}
	case "rag":
		// rag toolset requires either a ref or inline rag_config
		if t.Ref == "" && t.RAGConfig == nil {
			return errors.New("rag toolset requires either ref or rag_config")
		}
		if t.RAGConfig != nil {
			if err := t.RAGConfig.validate(); err != nil {
				return err
			}
		}
	case "background_agents":
		// no additional validation needed
	}

	return nil
}

// validate rejects an invalid RAGConfig. Currently only IndexingTimeout is
// checked; a negative value would make context.WithTimeout fire immediately.
func (c *RAGConfig) validate() error {
	if c.IndexingTimeout != nil && c.IndexingTimeout.Duration < 0 {
		return errors.New("indexing_timeout must not be negative")
	}
	return nil
}

// validateNonEmptyEntries rejects empty / whitespace-only entries in a
// generic string list (e.g. the mcp_catalog allow/block server lists). An
// empty id would never match a catalog server but signals a config typo.
func validateNonEmptyEntries(field string, entries []string) error {
	for i, e := range entries {
		if strings.TrimSpace(e) == "" {
			return fmt.Errorf("%s[%d] must not be empty", field, i)
		}
	}
	return nil
}

// validatePathRootEntries rejects empty / whitespace-only entries in a
// filesystem allow- or deny-list. An empty entry would be a foot-gun: it
// would resolve to the working directory and silently widen (or close) the
// matched set in surprising ways.
func validatePathRootEntries(field string, entries []string) error {
	for i, e := range entries {
		if strings.TrimSpace(e) == "" {
			return fmt.Errorf("%s[%d] must not be empty", field, i)
		}
	}
	return nil
}

// validateDomainPatterns rejects empty / whitespace-only entries and
// malformed wildcard or CIDR patterns in a fetch allow- or block-list.
//
// Catching these at config-load time turns silent foot-guns (e.g.
// `allowed_domains: [""]` rejecting every URL, `*.foo.*` matching nothing)
// into actionable errors. Plain hostnames and the leading-dot subdomain form
// are intentionally not validated for syntax — the matcher is purely
// string-based and any non-conforming entry simply never matches.
func validateDomainPatterns(field string, patterns []string) error {
	for i, p := range patterns {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			return fmt.Errorf("%s[%d] must not be empty", field, i)
		}
		if err := validateDomainPattern(trimmed); err != nil {
			return fmt.Errorf("%s[%d] %q is invalid: %w", field, i, p, err)
		}
	}
	return nil
}

// validateDomainPattern checks a single (already trimmed, non-empty) entry.
func validateDomainPattern(p string) error {
	// CIDR notation: must parse cleanly. We deliberately accept any /-bearing
	// string as "intended to be a CIDR" so a typo like "10.0.0.0/33" is
	// reported instead of being silently treated as a hostname.
	if strings.Contains(p, "/") {
		if _, _, err := net.ParseCIDR(p); err != nil {
			return fmt.Errorf("not a valid CIDR: %w", err)
		}
		return nil
	}
	// Wildcards: only the leading "*." form is supported. Anything else
	// ("foo.*", "*foo*", "**.example.com") would silently match nothing
	// under the current matcher, which is almost never what the user wants.
	if strings.Contains(p, "*") {
		rest, ok := strings.CutPrefix(p, "*.")
		if !ok || rest == "" || strings.Contains(rest, "*") {
			return errors.New("'*' is only allowed as a leading '*.' wildcard, e.g. '*.example.com'")
		}
	}
	return nil
}

// isLoopbackHost reports whether host is a loopback address (with or without
// a port component). It accepts IPv4 loopback, IPv6 loopback, and the literal
// "localhost".
func isLoopbackHost(hostPort string) bool {
	host := hostPort
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]") // strip IPv6 brackets
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// validateCallbackRedirectURL ensures raw is a well-formed absolute URL
// suitable for use as an OAuth redirect_uri.
//
// Rules:
//   - Must parse as an absolute URL (scheme + host) once the ${callbackPort}
//     placeholder has been substituted with a dummy value.
//   - Scheme must be http or https. Other schemes (javascript:, file:, ftp:,
//     …) are rejected: the browser will be navigated to this URL by the
//     authorization server.
//   - http is only permitted for loopback hosts (RFC 8252 §7.3); any other
//     host must use https, since non-loopback http redirect URIs allow the
//     authorization code to be exposed on the wire.
func validateCallbackRedirectURL(raw string) error {
	// Substitute the placeholder with a dummy port so url.Parse accepts the
	// string (Go's parser validates that ports are numeric).
	probe := strings.ReplaceAll(raw, "${callbackPort}", "1")
	u, err := url.Parse(probe)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("oauth callbackRedirectURL must be an absolute URL: %q", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("oauth callbackRedirectURL scheme must be http or https, got %q", u.Scheme)
	}
	if scheme == "http" && !isLoopbackHost(u.Host) {
		return fmt.Errorf("oauth callbackRedirectURL must use https for non-loopback hosts: %q", raw)
	}
	return nil
}
