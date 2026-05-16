package v8

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
	return t.validate()
}

func (t *Config) validate() error {
	for name, p := range t.Providers {
		if err := p.Auth.validate(p.Provider); err != nil {
			return fmt.Errorf("providers.%s: %w", name, err)
		}
	}
	for name, m := range t.Models {
		if err := m.Auth.validate(EffectiveProviderType(m, t.Providers)); err != nil {
			return fmt.Errorf("models.%s: %w", name, err)
		}
	}

	for i := range t.Agents {
		agent := &t.Agents[i]

		// Validate fallback config
		if err := agent.validateFallback(); err != nil {
			return err
		}

		for j := range agent.Toolsets {
			if err := agent.Toolsets[j].validate(); err != nil {
				return err
			}
		}
		if agent.Hooks != nil {
			if err := agent.Hooks.validate(); err != nil {
				return err
			}
		}
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

func (t *Toolset) validate() error {
	// Attributes used on the wrong toolset type.
	if len(t.Shell) > 0 && t.Type != "script" {
		return errors.New("shell can only be used with type 'script'")
	}
	if t.Path != "" && t.Type != "memory" && t.Type != "tasks" {
		return errors.New("path can only be used with type 'memory' or 'tasks'")
	}
	if len(t.PostEdit) > 0 && t.Type != "filesystem" {
		return errors.New("post_edit can only be used with type 'filesystem'")
	}
	if t.IgnoreVCS != nil && t.Type != "filesystem" {
		return errors.New("ignore_vcs can only be used with type 'filesystem'")
	}
	if len(t.AllowList) > 0 && t.Type != "filesystem" {
		return errors.New("allow_list can only be used with type 'filesystem'")
	}
	if len(t.DenyList) > 0 && t.Type != "filesystem" {
		return errors.New("deny_list can only be used with type 'filesystem'")
	}
	if err := validatePathRootEntries("allow_list", t.AllowList); err != nil {
		return err
	}
	if err := validatePathRootEntries("deny_list", t.DenyList); err != nil {
		return err
	}
	if len(t.Env) > 0 && (t.Type != "shell" && t.Type != "script" && t.Type != "mcp" && t.Type != "lsp") {
		return errors.New("env can only be used with type 'shell', 'script', 'mcp' or 'lsp'")
	}
	if len(t.FileTypes) > 0 && t.Type != "lsp" {
		return errors.New("file_types can only be used with type 'lsp'")
	}
	if len(t.AllowedDomains) > 0 && t.Type != "fetch" {
		return errors.New("allowed_domains can only be used with type 'fetch'")
	}
	if len(t.BlockedDomains) > 0 && t.Type != "fetch" {
		return errors.New("blocked_domains can only be used with type 'fetch'")
	}
	if t.AllowPrivateIPs && t.Type != "fetch" {
		return errors.New("allow_private_ips can only be used with type 'fetch'")
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
	if t.URL != "" && t.Type != "a2a" && t.Type != "openapi" {
		return errors.New("url can only be used with type 'a2a' or 'openapi'")
	}
	if t.Name != "" && (t.Type != "mcp" && t.Type != "a2a" && t.Type != "rag") {
		return errors.New("name can only be used with type 'mcp', 'a2a', or 'rag'")
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
		if t.Remote.OAuth != nil {
			if t.Remote.URL == "" {
				return errors.New("oauth requires remote url to be set")
			}
			if t.Remote.OAuth.ClientID == "" {
				return errors.New("oauth requires clientId to be set")
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
	case "model_picker":
		if len(t.Models) == 0 {
			return errors.New("model_picker toolset requires at least one model in the 'models' list")
		}
	case "rag":
		// rag toolset requires either a ref or inline rag_config
		if t.Ref == "" && t.RAGConfig == nil {
			return errors.New("rag toolset requires either ref or rag_config")
		}
	case "background_agents":
		// no additional validation needed
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
