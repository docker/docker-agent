package root

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/docker/cli/cli"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/docker/docker-agent/pkg/config"
	latestcfg "github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/sandbox"
	"github.com/docker/docker-agent/pkg/sandbox/kit"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// resolveSandboxDefault decides whether the sandbox path should be
// taken when the user did not pass --sandbox on the CLI. The first
// source that declares sandbox: true wins; in priority order:
//
//  1. an alias entry (`docker agent alias add ... --sandbox`);
//  2. the agent's own `runtime.sandbox: true`.
//
// Callers must only invoke this when the CLI flag was not set; an
// explicit --sandbox=<bool> always wins and bypasses this logic.
//
// The agent config (if any) loaded along the way is returned so
// runInSandbox can reuse it without paying the resolve+load cost a
// second time. cfg is nil when agentRef is empty or fails to load.
func resolveSandboxDefault(ctx context.Context, agentRef string, current bool) (bool, *latestcfg.Config) {
	if agentRef == "" {
		return current, nil
	}
	cfg := loadAgentConfig(ctx, agentRef)
	if current {
		return current, cfg
	}
	if alias := config.ResolveAlias(agentRef); alias != nil && alias.Sandbox {
		return true, cfg
	}
	return cfg != nil && cfg.Runtime != nil && cfg.Runtime.Sandbox, cfg
}

// agentNetworkAllowlist returns the hostnames the agent declared in
// runtime.network_allowlist. Entries with embedded commas or
// whitespace are dropped with a warning so a single malformed value
// can't smuggle several rules into the proxy policy. Returns nil
// when cfg is nil, has no Runtime block, or has no allowlist.
func agentNetworkAllowlist(ctx context.Context, cfg *latestcfg.Config) []string {
	if cfg == nil || cfg.Runtime == nil {
		return nil
	}
	var valid []string
	for _, h := range cfg.Runtime.NetworkAllowlist {
		if strings.ContainsAny(h, ", \t") {
			slog.WarnContext(ctx, "Ignoring invalid network_allowlist entry; contains comma or whitespace",
				"host", h)
			continue
		}
		valid = append(valid, h)
	}
	return valid
}

// loadAgentConfig is the shared best-effort loader: it resolves
// agentRef and loads the YAML, returning nil on any failure so
// callers fall through to the normal path that will surface a
// proper error from the eventual load.
func loadAgentConfig(ctx context.Context, agentRef string) *latestcfg.Config {
	if agentRef == "" {
		return nil
	}
	source, err := config.Resolve(agentRef, nil)
	if err != nil {
		return nil
	}
	cfg, err := config.Load(ctx, source)
	if err != nil {
		return nil
	}
	return cfg
}

// userSandboxAllowlist returns the persistent host list the user has
// taught docker-agent to open via `docker agent sandbox allow`.
// Best-effort: a missing or unreadable user config returns nil so
// the sandbox falls back to the inferred set only.
func userSandboxAllowlist(ctx context.Context) []string {
	cfg, err := userconfig.Load()
	if err != nil {
		slog.DebugContext(ctx, "Failed to load user config; skipping persistent sandbox allowlist", "error", err)
		return nil
	}
	return cfg.SandboxAllowlist
}

// runInSandbox delegates the current command to a Docker sandbox.
// It ensures a sandbox exists (creating or recreating as needed), then
// executes docker agent inside it via the sandbox exec command.
//
// agentCfg, when non-nil, is the parsed agent config already loaded by
// resolveSandboxDefault and is used to read runtime.network_allowlist
// without re-resolving the ref.
func runInSandbox(ctx context.Context, cmd *cobra.Command, args []string, runConfig *config.RuntimeConfig, template string, preferSbx, noKit bool, agentCfg *latestcfg.Config) error {
	if environment.InSandbox() {
		return fmt.Errorf("already running inside a Docker sandbox (VM %s)", os.Getenv("SANDBOX_VM_ID"))
	}

	backend := sandbox.NewBackend(preferSbx)

	if err := backend.CheckAvailable(ctx); err != nil {
		return err
	}

	var agentRef string
	if len(args) > 0 {
		agentRef = args[0]
	}

	configDir := paths.GetConfigDir()
	dockerAgentArgs := dockerAgentArgs(cmd, args, configDir)

	stopTokenWriter := sandbox.StartTokenWriterIfNeeded(ctx, configDir, runConfig.ModelsGateway)
	defer stopTokenWriter()

	// Resolve wd to an absolute path so that it matches the absolute
	// workspace paths returned by `docker sandbox ls --json`.
	wd, err := filepath.Abs(cmp.Or(runConfig.WorkingDir, "."))
	if err != nil {
		return fmt.Errorf("resolving workspace path: %w", err)
	}

	envProvider := environment.NewDefaultProvider()

	extras := []string{sandbox.ExtraWorkspace(wd, agentRef)}

	var kitResult *kit.Result
	if !noKit && agentRef != "" {
		kitResult, err = kit.Build(ctx, kit.Options{
			AgentRef:    agentRef,
			EnvProvider: envProvider,
			HostCwd:     wd,
			Workspace:   wd,
		})
		if err != nil {
			slog.WarnContext(ctx, "docker-agent kit build failed; continuing without kit", "error", err)
		} else {
			kitResult.PrintSummary(cmd.OutOrStdout())
			extras = append(extras, kitResult.HostDir)
			// We deliberately keep the kit on disk between runs:
			// the docker sandbox we reuse across runs holds a hard
			// reference to the kit's bind-mount path — deleting the
			// dir would leave the sandbox un-startable. The kit lives
			// in the cache dir keyed on a content hash, so the next
			// run for the same agent overwrites it in place; disk
			// usage is bounded by the number of distinct agents the
			// user has run.
		}
	}

	agentHosts := agentNetworkAllowlist(ctx, agentCfg)
	userHosts := userSandboxAllowlist(ctx)

	printModelsGateway(cmd.OutOrStdout(), runConfig.ModelsGateway)
	printModelsDevAllowance(cmd.OutOrStdout())
	printToolInstallAllowance(cmd.OutOrStdout(), kitResult)
	printAgentNetworkAllowlist(cmd.OutOrStdout(), agentHosts)
	printUserSandboxAllowlist(cmd.OutOrStdout(), userHosts)

	name, err := backend.Ensure(ctx, wd, extras, template, configDir)
	if err != nil {
		return err
	}

	// Sandbox templates ship with a default-deny network proxy that
	// allows the major model providers (api.anthropic.com, api.openai.com,
	// ...) but blocks every *.docker.com host as well as every
	// package-registry / source-host the auto-installer reaches for.
	// Open the minimum: the configured Docker AI gateway when set, and
	// the per-toolset package hosts the kit-build resolved against the
	// aqua registry. The kit narrows by package type (Go module proxy
	// for go_install, GitHub releases for github_release) so we don't
	// open holes for hosts the agent doesn't actually need.
	var toolHosts []string
	if kitResult != nil {
		toolHosts = kitResult.ToolInstallHosts
	}
	allowSandboxHosts(ctx, backend, name, runConfig.ModelsGateway, toolHosts, agentHosts, userHosts)

	// Resolve env vars the agent needs and forward them into the sandbox.
	// Docker Desktop proxies well-known API keys automatically; this handles
	// any additional vars (e.g. MCP tool secrets).
	envFlags, envVars := sandbox.EnvForAgent(ctx, agentRef, envProvider)

	// Forward the gateway by name so a URL with credentials never
	// shows up in the slog'd `docker sandbox exec` argv. We do not
	// forward DOCKER_TOKEN: inside the sandbox it must come only from
	// sandbox-tokens.json (kept fresh by StartTokenWriterIfNeeded).
	if gateway := runConfig.ModelsGateway; gateway != "" {
		envFlags = append(envFlags, "-e", envModelsGateway)
		envVars = append(envVars, envModelsGateway+"="+gateway)
	}

	// Point the in-sandbox resolvers at the staged kit. The sandbox CLI
	// exposes extra workspaces at the same path as on the host, so we
	// forward HostDir verbatim. We use the `-e KEY=VALUE` form so the
	// value is set directly inside the container; we deliberately do not
	// append it to envVars (which would set it on the host docker CLI
	// process too — a path that only makes sense inside the sandbox).
	if kitResult != nil {
		envFlags = append(envFlags, "-e", skills.KitDirEnv+"="+kitResult.HostDir)
	}

	// Wrap the sandbox exec in a span so the host side captures timing
	// and exit code, and inject W3C trace context via env vars so the
	// agent process spawned inside the sandbox container chains its
	// own spans onto this parent.
	ctx, sbxSpan := genai.StartSandboxExec(ctx, genai.SandboxOptions{
		Runtime:   "docker",
		Container: name,
	})
	defer sbxSpan.End()
	envFlags = append(envFlags, genai.InjectSandboxEnv(ctx)...)

	dockerCmd := backend.BuildExecCmd(ctx, name, wd, dockerAgentArgs, envFlags, envVars)
	slog.DebugContext(ctx, "Executing in sandbox", "name", name, "args", dockerCmd.Args)

	if err := dockerCmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			sbxSpan.SetExitCode(exitErr.ExitCode())
			sbxSpan.RecordError(err, "")
			return cli.StatusError{StatusCode: exitErr.ExitCode()}
		}
		sbxSpan.RecordError(err, "")
		return fmt.Errorf("docker sandbox exec failed: %w", err)
	}
	sbxSpan.SetExitCode(0)

	return nil
}

func dockerAgentArgs(cmd *cobra.Command, args []string, configDir string) []string {
	skip := map[string]bool{
		"sandbox":    true,
		"sbx":        true,
		"config-dir": true,
		"no-kit":     true,
	}

	var dockerAgentArgs []string
	hasYolo := false
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if skip[f.Name] {
			return
		}

		if f.Name == "yolo" {
			hasYolo = true
		}

		if f.Value.Type() == "bool" {
			dockerAgentArgs = append(dockerAgentArgs, "--"+f.Name+"="+f.Value.String())
		} else {
			dockerAgentArgs = append(dockerAgentArgs, "--"+f.Name, f.Value.String())
		}
	})
	if !hasYolo {
		dockerAgentArgs = append(dockerAgentArgs, "--yolo")
	}

	dockerAgentArgs = append(dockerAgentArgs, args...)
	dockerAgentArgs = append(dockerAgentArgs, "--config-dir", configDir)

	return dockerAgentArgs
}

// allowSandboxHosts adds per-sandbox allow-network rules for every
// host the in-sandbox runtime is known to need: the models.dev
// catalog API (always), the configured models gateway (when set), the
// package hosts the auto-installer reaches for (when the kit build
// identified at least one auto-installable toolset), and any extra
// hosts the agent author declared in runtime.network_allowlist. The
// default sandbox proxy denies all of them; without this, the inner
// agent's first request returns a misleading "403 Blocked by network
// policy".
//
// Holes are punched only when the corresponding feature is in play:
//   - models.dev is always opened — every run resolves model
//     metadata (limits, pricing, capabilities) against the catalog;
//   - the gateway host is added only when gatewayURL is non-empty;
//   - the per-agent install hosts come from the kit build, which
//     looks each toolset up against the aqua registry and contributes
//     only the hosts that toolset's install path actually uses (Go
//     module proxy + toolchain bootstrap for go_install packages,
//     GitHub release hosts for github_release packages). When a
//     lookup failed, the kit folds in [toolinstall.FallbackHosts]
//     so the run can still succeed;
//   - the agent-declared hosts come straight from the YAML and are
//     unioned with the inferred set so authors can add hosts the
//     resolver doesn't know about (custom MCP endpoints, third-party
//     APIs, ...).
//
// Best-effort: a malformed gateway URL or a backend that doesn't
// support per-sandbox policies is logged at debug level and the run
// proceeds. The user will then see a network-policy 403 from the
// inner and we surface that diagnostic verbatim.
func allowSandboxHosts(ctx context.Context, backend *sandbox.Backend, name, gatewayURL string, toolInstallHosts, agentHosts, userHosts []string) {
	var hosts []string
	hosts = append(hosts, modelsdev.APIHost)
	hosts = append(hosts, toolInstallHosts...)
	hosts = append(hosts, agentHosts...)
	hosts = append(hosts, userHosts...)

	if gatewayURL != "" {
		if h := gatewayHostPort(gatewayURL); h != "" {
			hosts = append(hosts, h)
		} else {
			slog.DebugContext(ctx, "Could not extract host from models-gateway URL; not allowlisting",
				"gateway", gatewayURL)
		}
	}

	if len(hosts) == 0 {
		return
	}
	if err := backend.AllowHosts(ctx, name, hosts); err != nil {
		slog.WarnContext(ctx, "Failed to allowlist sandbox hosts; the inner agent may see HTTP 403",
			"sandbox", name, "hosts", hosts, "error", err)
	}
}

// gatewayHostPort returns the "host" or "host:port" portion of a
// gateway URL, or "" if rawURL doesn't carry a usable authority.
//
// We accept both fully formed URLs (https://example.com:443/proxy),
// scheme-relative authorities (//example.com), and bare authorities
// (example.com:443) so users can configure the gateway either way.
// IPv6 brackets, ports, paths, queries and fragments are stripped
// from the returned value; malformed inputs (no host, scheme without
// host, opaque non-HTTP schemes, etc.) return "".
func gatewayHostPort(rawURL string) string {
	if rawURL == "" {
		return ""
	}

	candidate := rawURL
	switch {
	case strings.HasPrefix(rawURL, "//"):
		// Protocol-relative authority — attach a fake scheme so url.Parse
		// recognises the host.
		candidate = "http:" + rawURL
	case hasURLScheme(rawURL):
		// Fully formed URL.
	case strings.Contains(rawURL, "://"):
		// Looks like a URL but the scheme part is bogus — reject.
		return ""
	case looksOpaque(rawURL):
		// e.g. "mailto:foo@example.com" — not a network endpoint.
		return ""
	default:
		// Bare authority. Strip trailing path / query / fragment up
		// front so url.Parse doesn't mistake "example.com:443/proxy"
		// for an opaque scheme.
		if i := strings.IndexAny(rawURL, "/?#"); i >= 0 {
			candidate = "http://" + rawURL[:i]
		} else {
			candidate = "http://" + rawURL
		}
	}

	u, err := url.Parse(candidate)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// hasURLScheme reports whether s looks like "scheme://..." with a
// non-empty scheme. We deliberately don't accept "scheme:opaque"
// (no //) because the gateway URL is always an HTTP(S) endpoint.
func hasURLScheme(s string) bool {
	i := strings.Index(s, "://")
	if i <= 0 {
		return false
	}
	return isSchemeChars(s[:i])
}

// looksOpaque reports whether s looks like an opaque-form URI
// ("scheme:rest", no //) such as "mailto:foo" or "data:...". These
// don't carry a network authority and must not be re-parsed as bare
// host strings.
func looksOpaque(s string) bool {
	i := strings.Index(s, ":")
	if i <= 0 {
		return false
	}
	// Reject anything with a port-looking right side, so values like
	// "example.com:443" are treated as bare authorities, not opaque.
	rhs := s[i+1:]
	if rhs != "" && rhs[0] >= '0' && rhs[0] <= '9' {
		return false
	}
	return isSchemeChars(s[:i])
}

// isSchemeChars reports whether s is a syntactically valid URL scheme
// (RFC 3986: ALPHA *( ALPHA / DIGIT / "+" / "-" / "." ), starting with ALPHA).
func isSchemeChars(s string) bool {
	if s == "" {
		return false
	}
	first := s[0]
	if (first < 'a' || first > 'z') && (first < 'A' || first > 'Z') {
		return false
	}
	for _, r := range s[1:] {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '+' || r == '-' || r == '.':
			continue
		default:
			return false
		}
	}
	return true
}

// displayGatewayURL returns a credential-safe rendering of a gateway
// URL: any user/password embedded in the authority is replaced with
// "***" so it never lands in stdout, slog output, or screenshots.
// Inputs that don't parse cleanly (or carry no userinfo) are returned
// unchanged.
//
// We rebuild the string by hand instead of calling u.String() because
// url.User on the latter URL-escapes the masking characters into
// %2A%2A%2A, which is technically correct but uglier than "***" in a
// human-facing line.
func displayGatewayURL(rawURL string) string {
	if rawURL == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil {
		return rawURL
	}

	var b strings.Builder
	if u.Scheme != "" {
		b.WriteString(u.Scheme)
		b.WriteString("://")
	}
	b.WriteString("***@")
	b.WriteString(u.Host)
	b.WriteString(u.RequestURI())
	if u.Fragment != "" {
		b.WriteByte('#')
		b.WriteString(u.Fragment)
	}
	return b.String()
}

// printModelsGateway prints which AI gateway (if any) the inner agent
// will route model requests through. Surfacing this between the kit
// summary and the sandbox-create line makes it obvious — at a glance
// — whether an "HTTP 403" the user might see later originated from
// the proxy's network policy (gateway host not yet allow-listed) or
// from the gateway server itself.
//
// An unset gateway means we did not configure routing through one;
// the inner agent will fall back to whatever the docker-agent process
// resolves from its own environment (DOCKER_TOKEN, OS env vars,
// Docker Model Runner, etc.).
func printModelsGateway(w io.Writer, gateway string) {
	if gateway == "" {
		fmt.Fprintln(w, "Models gateway: none configured")
		return
	}
	display := displayGatewayURL(gateway)
	host := gatewayHostPort(gateway)
	if host == "" || host == gateway {
		fmt.Fprintf(w, "Models gateway: %s\n", display)
		return
	}
	fmt.Fprintf(w, "Models gateway: %s (allowlisting %s in the sandbox proxy)\n", display, host)
}

// printModelsDevAllowance prints that the models.dev catalog host is
// always allowlisted in the sandbox proxy, since every run resolves
// model metadata against it.
func printModelsDevAllowance(w io.Writer) {
	fmt.Fprintf(w, "Models catalog: allowlisting %s in the sandbox proxy\n", modelsdev.APIHost)
}

// printToolInstallAllowance prints a multi-line description of the
// package-host allowlist opened for this sandbox: a one-liner
// summary followed by every host on its own indented line so the
// user can see exactly what holes the run punched in the default-
// deny network policy. Silent when the kit isn't built or when no
// auto-installable toolset was detected.
//
// When per-toolset registry resolution failed for at least one
// toolset, a best-effort fallback union was used instead and a
// warning line names each unresolved toolset so the user can spot
// why the allowlist is wider than expected.
func printToolInstallAllowance(w io.Writer, kitResult *kit.Result) {
	if kitResult == nil || !kitResult.NeedsToolInstall {
		return
	}
	fmt.Fprintf(w, "Tool install: agent has at least one MCP/LSP toolset, allowlisting %d package host(s) in the sandbox proxy:\n",
		len(kitResult.ToolInstallHosts))
	for _, h := range kitResult.ToolInstallHosts {
		fmt.Fprintf(w, "  - %s\n", h)
	}
	for _, e := range kitResult.ToolInstallHostsResolutionErr {
		fmt.Fprintf(w, "  ! %s (using fallback host set)\n", e.Error())
	}
	if len(kitResult.ToolInstallHostsResolutionErr) > 0 {
		fmt.Fprintln(w, "  hint: persist a missing host with `docker agent sandbox allow <host>`")
	}
}

// printAgentNetworkAllowlist prints the host(s) the agent's config
// asked us to add to the sandbox proxy. Surfacing them next to the
// kit / gateway lines makes it obvious which holes were punched by
// the agent author vs auto-discovered, so an unexpected 403 has a
// short list of suspects.
func printAgentNetworkAllowlist(w io.Writer, hosts []string) {
	if len(hosts) == 0 {
		return
	}
	fmt.Fprintf(w, "Agent network allowlist: allowlisting %d host(s) declared in runtime.network_allowlist:\n", len(hosts))
	for _, h := range hosts {
		fmt.Fprintf(w, "  - %s\n", h)
	}
}

// printUserSandboxAllowlist prints the host(s) the user has added
// via `docker agent sandbox allow`. Kept on its own line (separate
// from the agent-declared list) so it's clear which hosts persist
// across runs vs which travel with the agent config.
func printUserSandboxAllowlist(w io.Writer, hosts []string) {
	if len(hosts) == 0 {
		return
	}
	fmt.Fprintf(w, "User sandbox allowlist: allowlisting %d host(s) from `docker agent sandbox allow`:\n", len(hosts))
	for _, h := range hosts {
		fmt.Fprintf(w, "  - %s\n", h)
	}
}
