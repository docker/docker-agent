package sandbox

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/docker/docker-agent/pkg/atomicfile"
	"github.com/docker/docker-agent/pkg/paths"
)

// LoginKit materialises a tiny sbx mixin kit declaring the reserved
// sbx-login credential service: the sandbox proxy injects the user's
// fresh Docker login JWT into HTTPS requests to the gateway host, and
// exports DOCKER_TOKEN inside the sandbox as a proxy-managed sentinel
// so docker-agent's sign-in preflight passes. The real token never
// enters the sandbox — the proxy only ever injects it into HTTPS
// requests to docker.com / *.docker.com hosts.
//
// The kit lives in a deterministic per-host directory under the cache
// dir; callers mount it read-only so its presence doubles as a reuse
// marker (see [Backend.Ensure]).
//
// Returns "" when gateway is empty or is not an HTTPS docker.com URL —
// any other gateway authenticates by its own means.
func LoginKit(gateway string, v3 bool) (string, error) {
	host := dockerGatewayHostname(gateway)
	if host == "" {
		return "", nil
	}

	dir := filepath.Join(loginKitParent(), host)
	if v3 {
		dir += "-v3"
	}
	// Only the host-side sandbox CLI reads the kit (at create time, as
	// the same user); nothing inside the sandbox needs it, so keep it
	// owner-only.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating login kit directory: %w", err)
	}

	spec := fmt.Sprintf(`schemaVersion: "2"
kind: mixin
name: docker-agent-gateway-auth
description: Authenticate Docker AI gateway requests with the user's Docker login

credentials:
  - service: sbx-login
    apiKey:
      name: DOCKER_TOKEN
      proxyManaged: true
      inject:
        - domain: %s
          header: Authorization
          format: "Bearer %%s"
`, host)

	filename := "spec.yaml"
	if v3 {
		filename = "gateway.yaml"
		spec = fmt.Sprintf(`# syntax=docker/sandbox-kit:3
schemaVersion: "3"
kind: mixin
displayName: Docker Agent gateway authentication
capabilities:
  - type: com.docker.sandbox/network-policy@1
    config:
      runtime:
        allow: [%s]
  - type: com.docker.sandbox/credential@1
    config:
      service: sbx-login
      phase: runtime
      apiKey:
        name: DOCKER_TOKEN
        proxyManaged: true
        inject:
          - domain: %s
            header: Authorization
            format: "Bearer %%s"
`, host, host)
	}

	// Atomic write: concurrent runs for the same gateway share this
	// path and `create` must never read a partial spec.
	if err := atomicfile.Write(filepath.Join(dir, filename), bytes.NewReader([]byte(spec)), 0o600); err != nil {
		return "", fmt.Errorf("writing login kit spec: %w", err)
	}
	return dir, nil
}

// loginKitParent is the directory holding all generated login kits,
// one sub-directory per gateway host.
func loginKitParent() string {
	return filepath.Join(paths.GetCacheDir(), "sandbox-login-kit")
}

// hostnamePattern matches a conventional DNS hostname: dot-separated
// labels of letters, digits and inner hyphens. url.Parse is far more
// permissive (it accepts YAML indicator characters like "!", "&" or
// "*"), and the hostname is interpolated into the kit YAML and used
// as a directory name, so anything unconventional is rejected.
var hostnamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// dockerGatewayHostname returns the hostname of gateway when it is an
// HTTPS URL on docker.com or a subdomain, and "" otherwise.
func dockerGatewayHostname(gateway string) string {
	if gateway == "" {
		return ""
	}
	u, err := url.Parse(gateway)
	if err != nil || u.Scheme != "https" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if !hostnamePattern.MatchString(host) {
		return ""
	}
	if host == "docker.com" || strings.HasSuffix(host, ".docker.com") {
		return host
	}
	return ""
}
