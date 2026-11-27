package openai

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"uuid"

	"github.com/docker/docker-agent/pkg/chatgpt"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

const (
	chatgptBetaHeader          = "responses=experimental"
	chatgptDefaultInstructions = "You are a helpful assistant."
)

func isChatGPTProvider(cfg *latest.ModelConfig) bool {
	return cfg != nil && cfg.Provider == chatgpt.ProviderName
}

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
			return "", fmt.Errorf("chatgpt provider: %w; sign in with `docker agent setup` or set %s", err, tokenKey)
		}
		return token, nil
	}
}

func chatgptAuthHeaderFunc(tokenSource func(context.Context) (string, error)) func(context.Context, http.Header) error {
	sessionID := uuid.NewV4().String()

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

	return func(ctx context.Context, h http.Header) error {
		token, err := tokenSource(ctx)
		if err != nil {
			return err
		}
		h.Set("Authorization", "Bearer "+token)
		if id := accountID(token); id != "" && h.Get("chatgpt-account-id") == "" {
			h.Set("chatgpt-account-id", id)
		}
		if h.Get("OpenAI-Beta") == "" {
			h.Set("OpenAI-Beta", chatgptBetaHeader)
		}
		if h.Get("originator") == "" {
			h.Set("originator", chatgpt.Originator)
		}
		if h.Get("session_id") == "" {
			h.Set("session_id", sessionID)
		}
		return nil
	}
}

func applyChatGPTResponsesPolicy(ctx context.Context, params *ResponseRequest) {
	params.Store = new(false)

	if instructions := extractSystemInstructions(params); instructions != "" {
		params.Instructions = new(instructions)
	} else {
		params.Instructions = new(chatgptDefaultInstructions)
	}

	if params.Temperature != nil || params.TopP != nil || params.MaxOutputTokens != nil {
		slog.DebugContext(ctx, "Dropping sampling parameters not supported by the ChatGPT backend",
			"temperature", params.Temperature != nil,
			"top_p", params.TopP != nil,
			"max_output_tokens", params.MaxOutputTokens != nil)
		params.Temperature = nil
		params.TopP = nil
		params.MaxOutputTokens = nil
	}

	if params.Reasoning != nil && params.Reasoning.Summary != "" {
		params.Reasoning.Summary = "auto"
	}
}

func extractSystemInstructions(params *ResponseRequest) string {
	items := params.Input
	var instructions []string
	kept := items[:0]
	for _, item := range items {
		if (item.Type == "" || item.Type == "message") && item.Role == "system" {
			if len(item.ContentParts) > 0 {
				for _, part := range item.ContentParts {
					if part.Type == "input_text" && strings.TrimSpace(part.Text) != "" {
						instructions = append(instructions, part.Text)
					}
				}
			} else if item.Content != nil && strings.TrimSpace(*item.Content) != "" {
				instructions = append(instructions, *item.Content)
			}
			continue
		}
		kept = append(kept, item)
	}
	params.Input = kept
	return strings.Join(instructions, "\n\n")
}
