package openai

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v3/option"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/httpclient"
)

// OpenCode asks clients to send one stable ID per conversation; it keys their
// routing and prompt caching.
const (
	opencodeSessionHeader = "x-opencode-session"
	opencodeHost          = "opencode.ai"
)

// opencodeSessionNamespace salts the hash so the header cannot be mapped back
// to the local session ID or the gateway's X-Cagent-Session-Id.
var opencodeSessionNamespace = uuid.MustParse("6f0c2a1e-8d4b-4c7f-9a3e-2b5d7e9f1c03")

// isOpenCodeProvider reports whether requests target OpenCode, either through
// the built-in aliases or a custom provider pointed at opencode.ai.
func isOpenCodeProvider(cfg *latest.ModelConfig) bool {
	if cfg == nil {
		return false
	}
	switch cfg.Provider {
	case "opencode-go", "opencode-zen":
		return true
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == opencodeHost || strings.HasSuffix(host, "."+opencodeHost)
}

// opencodeSessionID derives the header from the agent session, so multiplexed
// deployments get one ID per conversation rather than per client, and a
// resumed session keeps its ID across processes.
func opencodeSessionID(sessionID string) string {
	return uuid.NewSHA1(opencodeSessionNamespace, []byte(sessionID)).String()
}

// opencodeSessionMiddleware sets the header per request, leaving a user-pinned
// value alone. Requests with no session on the context share a per-client ID.
func opencodeSessionMiddleware() option.Middleware {
	fallback := uuid.NewString()

	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		if req.Header.Get(opencodeSessionHeader) == "" {
			id := fallback
			if sid := httpclient.SessionIDFromContext(req.Context()); sid != "" {
				id = opencodeSessionID(sid)
			}
			req.Header.Set(opencodeSessionHeader, id)
		}
		return next(req)
	}
}
