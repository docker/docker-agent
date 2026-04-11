package openaicodex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/codexauth"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	openairesponses "github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	codexBaseURL    = "https://chatgpt.com/backend-api/codex"
	codexOriginator = "codex_cli_rs"
)

type Client struct {
	base.Config
	clientFn func() (*oai.Client, error)
}

// NewClient creates a client for the ChatGPT-backed Codex provider.
func NewClient(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (*Client, error) {
	if cfg == nil {
		return nil, errors.New("model configuration is required")
	}
	if env == nil {
		return nil, errors.New("environment provider is required")
	}

	var globalOptions options.ModelOptions
	for _, opt := range opts {
		opt(&globalOptions)
	}

	clientFn := func() (*oai.Client, error) {
		auth, err := codexauth.Load()
		if err != nil {
			return nil, fmt.Errorf("load Codex auth: %w", err)
		}
		if !auth.HasChatGPTAuth() {
			return nil, errors.New("Codex ChatGPT login not found; run `codex login` first")
		}

		accountID := auth.AccountID()
		if accountID == "" {
			return nil, errors.New("Codex auth is missing chatgpt_account_id")
		}

		httpClient := httpclient.NewHTTPClient(ctx,
			httpclient.WithHeader("Authorization", "Bearer "+auth.Tokens.AccessToken),
			httpclient.WithHeader("chatgpt-account-id", accountID),
			httpclient.WithHeader("OpenAI-Beta", "responses=experimental"),
			httpclient.WithHeader("originator", codexOriginator),
		)

		client := oai.NewClient(
			option.WithAPIKey("chatgpt-oauth"),
			option.WithBaseURL(codexBaseURL),
			option.WithHTTPClient(httpClient),
		)
		return &client, nil
	}

	return &Client{
		Config: base.Config{
			ModelConfig:  *cfg,
			ModelOptions: globalOptions,
			Env:          env,
		},
		clientFn: clientFn,
	}, nil
}

// CreateChatCompletionStream creates a streaming response request for the Codex backend.
func (c *Client) CreateChatCompletionStream(
	ctx context.Context,
	messages []chat.Message,
	requestTools []tools.Tool,
) (chat.MessageStream, error) {
	if len(messages) == 0 {
		return nil, errors.New("at least one message is required")
	}

	params := responses.ResponseNewParams{
		Model: c.ModelConfig.Model,
		Store: param.NewOpt(false),
		Include: []responses.ResponseIncludable{
			"reasoning.encrypted_content",
		},
	}
	instructions, inputMessages := splitInstructions(messages)
	if instructions != "" {
		params.Instructions = param.NewOpt(instructions)
	}
	params.Input.OfInputItemList = openairesponses.ConvertMessagesToResponseInput(inputMessages)

	if c.ModelConfig.Temperature != nil {
		params.Temperature = param.NewOpt(*c.ModelConfig.Temperature)
	}
	if c.ModelConfig.TopP != nil {
		params.TopP = param.NewOpt(*c.ModelConfig.TopP)
	}
	if len(requestTools) > 0 {
		toolsParam := make([]responses.ToolUnionParam, len(requestTools))
		for i, tool := range requestTools {
			parameters, err := openairesponses.ConvertParametersToSchema(tool.Parameters)
			if err != nil {
				return nil, err
			}
			toolsParam[i] = responses.ToolUnionParam{
				OfFunction: &responses.FunctionToolParam{
					Name:        tool.Name,
					Description: param.NewOpt(tool.Description),
					Parameters:  parameters,
					Strict:      param.NewOpt(true),
				},
			}
		}
		params.Tools = toolsParam
		if c.ModelConfig.ParallelToolCalls != nil {
			params.ParallelToolCalls = param.NewOpt(*c.ModelConfig.ParallelToolCalls)
		}
	}

	if !c.ModelOptions.NoThinking() {
		params.Reasoning = shared.ReasoningParam{
			Summary: shared.ReasoningSummaryAuto,
		}
		if c.ModelConfig.ThinkingBudget != nil {
			effortStr, err := openAIReasoningEffort(c.ModelConfig.ThinkingBudget)
			if err != nil {
				return nil, err
			}
			params.Reasoning.Effort = shared.ReasoningEffort(effortStr)
		}
	}

	if structuredOutput := c.ModelOptions.StructuredOutput(); structuredOutput != nil {
		params.Text.Format.OfJSONSchema = &responses.ResponseFormatTextJSONSchemaConfigParam{
			Name:        structuredOutput.Name,
			Description: param.NewOpt(structuredOutput.Description),
			Schema:      structuredOutput.Schema,
			Strict:      param.NewOpt(structuredOutput.Strict),
		}
	}

	if requestJSON, err := json.Marshal(params); err == nil {
		slog.Debug("OpenAI Codex responses request", "request", string(requestJSON))
	}

	client, err := c.clientFn()
	if err != nil {
		return nil, err
	}

	trackUsage := c.ModelConfig.TrackUsage == nil || *c.ModelConfig.TrackUsage
	stream := client.Responses.NewStreaming(ctx, params)
	return openairesponses.NewResponseSSEAdapter(stream, trackUsage), nil
}

// splitInstructions moves system messages into the top-level instructions field.
func splitInstructions(messages []chat.Message) (string, []chat.Message) {
	var instructions []string
	inputMessages := make([]chat.Message, 0, len(messages))

	for _, msg := range messages {
		if msg.Role != chat.MessageRoleSystem {
			inputMessages = append(inputMessages, msg)
			continue
		}

		text := strings.TrimSpace(msg.Content)
		if text == "" && len(msg.MultiContent) > 0 {
			var parts []string
			for _, part := range msg.MultiContent {
				if part.Type == chat.MessagePartTypeText && strings.TrimSpace(part.Text) != "" {
					parts = append(parts, strings.TrimSpace(part.Text))
				}
			}
			text = strings.Join(parts, "\n\n")
		}

		if text != "" {
			instructions = append(instructions, text)
		}
	}

	return strings.Join(instructions, "\n\n"), inputMessages
}

func openAIReasoningEffort(b *latest.ThinkingBudget) (string, error) {
	if b == nil {
		return string(effort.Medium), nil
	}
	level, ok := b.EffortLevel()
	if !ok {
		return "", fmt.Errorf("openai-codex provider expects string reasoning effort, got token budget")
	}
	switch level {
	case effort.None, effort.Minimal, effort.Low, effort.Medium, effort.High, effort.XHigh:
		return string(level), nil
	default:
		return "", fmt.Errorf("invalid openai-codex thinking_budget %q", level)
	}
}
