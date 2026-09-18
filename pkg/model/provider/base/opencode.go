package base

import (
	"cmp"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/httpclient"
)

// OpenCodeSessionHeader carries the stable per-conversation ID OpenCode asks
// clients to send for routing and prompt caching.
const OpenCodeSessionHeader = "x-opencode-session"

const opencodeHost = "opencode.ai"

// Salts the hash so the header cannot be mapped back to the session ID.
var opencodeSessionNamespace = uuid.MustParse("6f0c2a1e-8d4b-4c7f-9a3e-2b5d7e9f1c03")

// IsOpenCodeProvider reports whether cfg's base URL points at opencode.ai.
func IsOpenCodeProvider(cfg *latest.ModelConfig) bool {
	if cfg == nil {
		return false
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == opencodeHost || strings.HasSuffix(host, "."+opencodeHost)
}

// WrapOpenCodeSession installs the session-header transport when cfg targets
// OpenCode. Call it after any transport wrapper so the wrapper sees the header.
// No-op for other providers and for a nil client (Vertex AI backends).
func WrapOpenCodeSession(cfg *latest.ModelConfig, client *http.Client) {
	if client == nil || !IsOpenCodeProvider(cfg) {
		return
	}
	client.Transport = &opencodeSessionTransport{
		base:     cmp.Or(client.Transport, http.DefaultTransport),
		fallback: uuid.NewString(),
	}
}

// opencodeSessionID derives one ID per conversation, stable across processes.
func opencodeSessionID(sessionID string) string {
	return uuid.NewSHA1(opencodeSessionNamespace, []byte(sessionID)).String()
}

// opencodeSessionTransport sets the header per request unless already set,
// from the session on the context or a per-client fallback.
type opencodeSessionTransport struct {
	base     http.RoundTripper
	fallback string
}

func (t *opencodeSessionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get(OpenCodeSessionHeader) != "" {
		return t.base.RoundTrip(req)
	}
	id := t.fallback
	if sid := httpclient.SessionIDFromContext(req.Context()); sid != "" {
		id = opencodeSessionID(sid)
	}
	// Never modify the caller's request.
	req = req.Clone(req.Context())
	req.Header.Set(OpenCodeSessionHeader, id)
	return t.base.RoundTrip(req)
}
