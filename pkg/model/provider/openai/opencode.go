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

// OpenCode Go/Zen require an x-opencode-session header carrying one stable ID
// per conversation; it is the key their edge uses for prompt-cache routing and
// requests without it may be rejected. See
// https://github.com/docker/docker-agent/issues/4164
const (
	opencodeSessionHeader = "x-opencode-session"
	opencodeHost          = "opencode.ai"
)

// opencodeSessionNamespace salts the session hash so the header value cannot
// be mapped back to the local session store ID or to the X-Cagent-Session-Id
// sent to the Docker gateway.
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

// opencodeSessionID derives the header value from the docker-agent session so
// every request of one conversation shares it, including across processes
// when a persisted session is resumed. Multiplexed deployments (serve api,
// serve chat) therefore get one ID per conversation rather than per client.
func opencodeSessionID(sessionID string) string {
	return uuid.NewSHA1(opencodeSessionNamespace, []byte(sessionID)).String()
}

// opencodeSessionMiddleware sets x-opencode-session on every request unless
// the user pinned one via provider_opts.http_headers. Requests without a
// session on the context (embeddings, one-off calls) share a per-client
// fallback ID so they still satisfy the requirement.
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
