package openai

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/docker/docker-agent/pkg/chatgpt"
	"github.com/docker/docker-agent/pkg/cli/invocation"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// The ChatGPT Codex backend authenticates with a ChatGPT account OAuth token
// instead of an API key and requires Codex-specific request headers. See
// pkg/chatgpt for the login flow and token refresh.
const (
	chatgptBetaHeader = "responses=experimental"

	// chatgptDefaultInstructions is sent when the conversation carries no
	// system message: the Codex backend rejects requests without an
	// instructions field.
	chatgptDefaultInstructions = "You are a helpful assistant."
)

// isChatGPTProvider reports whether the model config targets the ChatGPT
// account (subscription) provider.
func isChatGPTProvider(cfg *latest.ModelConfig) bool {
	return cfg != nil && cfg.Provider == chatgpt.ProviderName
}

// chatgptTokenSource resolves the access token for every request: the
// configured env var wins (the default chain serves the stored login through
// it, and an explicit CHATGPT_OAUTH_TOKEN overrides it), with a direct
// fallback to the stored login for embedders whose environment chain does
// not include the chatgpt-login source.
func chatgptTokenSource(env environment.Provider, tokenKey string) func(context.Context) (string, error) {
	if tokenKey == "" {
		tokenKey = chatgpt.TokenEnvVar
	}
	return func(ctx context.Context) (string, error) {
		if token, _ := env.Get(ctx, tokenKey); token != "" {
			return token, nil
		}
		token, err := chatgpt.AccessToken(ctx)
		if err != nil {
			return "", fmt.Errorf("chatgpt provider: %w; sign in with `%s setup` or set %s", err, invocation.DockerAgent(), tokenKey)
		}
		return token, nil
	}
}

// chatgptAuthMiddleware injects the Codex backend auth headers on every
// request, re-resolving the token so a long session survives access-token
// expiry. Headers already set by the user (provider_opts.http_headers) win,
// except Authorization, which must always carry a fresh token.
func chatgptAuthMiddleware(tokenSource func(context.Context) (string, error)) option.Middleware {
	sessionID := uuid.NewString()

	// The account id lives in the JWT and only changes when the token does,
	// so memoize the parse instead of decoding on every request.
	var mu sync.Mutex
	var lastToken, lastAccountID string

	accountID := func(token string) string {
		mu.Lock()
		defer mu.Unlock()
		if token != lastToken {
			lastToken, lastAccountID = token, chatgpt.AccountIDFromToken(token)
		}
		return lastAccountID
	}

	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		token, err := tokenSource(req.Context())
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if id := accountID(token); id != "" && req.Header.Get("chatgpt-account-id") == "" {
			req.Header.Set("chatgpt-account-id", id)
		}
		if req.Header.Get("OpenAI-Beta") == "" {
			req.Header.Set("OpenAI-Beta", chatgptBetaHeader)
		}
		if req.Header.Get("originator") == "" {
			req.Header.Set("originator", chatgpt.Originator)
		}
		if req.Header.Get("session_id") == "" {
			req.Header.Set("session_id", sessionID)
		}
		return next(req)
	}
}

// applyChatGPTResponsesPolicy adapts a Responses API request to the ChatGPT
// Codex backend, which is stricter than the platform API:
//   - store must be false (the backend is stateless for third-party clients);
//   - instructions are required, so system messages move into that field;
//   - client-side sampling and output caps (temperature, top_p,
//     max_output_tokens) are rejected for the served models and are dropped;
//   - reasoning summaries must use the "auto" granularity.
func applyChatGPTResponsesPolicy(ctx context.Context, params *responses.ResponseNewParams) {
	params.Store = param.NewOpt(false)

	if instructions := extractSystemInstructions(params); instructions != "" {
		params.Instructions = param.NewOpt(instructions)
	} else {
		params.Instructions = param.NewOpt(chatgptDefaultInstructions)
	}

	if params.Temperature.Valid() || params.TopP.Valid() || params.MaxOutputTokens.Valid() {
		slog.DebugContext(ctx, "Dropping sampling parameters not supported by the ChatGPT backend",
			"temperature", params.Temperature.Valid(),
			"top_p", params.TopP.Valid(),
			"max_output_tokens", params.MaxOutputTokens.Valid())
		params.Temperature = param.Opt[float64]{}
		params.TopP = param.Opt[float64]{}
		params.MaxOutputTokens = param.Opt[int64]{}
	}

	if params.Reasoning.Summary != "" {
		params.Reasoning.Summary = shared.ReasoningSummaryAuto
	}
}

// extractSystemInstructions removes every system message from the request
// input and returns their concatenated text. docker-agent emits one system
// message per source (agent instruction plus each toolset's instructions);
// the Codex backend expects all of them in the top-level instructions field.
func extractSystemInstructions(params *responses.ResponseNewParams) string {
	items := params.Input.OfInputItemList
	var instructions []string
	kept := items[:0]
	for _, item := range items {
		if item.OfInputMessage != nil && item.OfInputMessage.Role == "system" {
			for _, part := range item.OfInputMessage.Content {
				if part.OfInputText != nil && strings.TrimSpace(part.OfInputText.Text) != "" {
					instructions = append(instructions, part.OfInputText.Text)
				}
			}
			continue
		}
		kept = append(kept, item)
	}
	params.Input.OfInputItemList = kept
	return strings.Join(instructions, "\n\n")
}
