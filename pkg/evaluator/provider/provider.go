// Package provider constructs independently configured evaluator clients.
package provider

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

const (
	defaultBaseURL = "https://api.typesafe.ai"
	openAIBaseURL  = "https://api.openai.com/v1"
	defaultTimeout = 10 * time.Second
)

// New builds a reusable client from a resolved evaluator configuration.
// Credentials are obtained from env on each evaluation, not during construction.
func New(ctx context.Context, cfg latest.EvaluatorConfig, env environment.Provider, opts ...options.Opt) (evaluator.Evaluator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, errors.New("invalid evaluator configuration")
	}
	var defaultURL, path, tokenKey string
	switch cfg.Provider {
	case "typesafe":
		defaultURL, path, tokenKey = defaultBaseURL, "/v1/systemone", "TYPESAFE_API_KEY"
	case "openai":
		defaultURL, path, tokenKey = openAIBaseURL, "/v1/decisions", "OPENAI_API_KEY"
	default:
		return nil, errors.New("unsupported evaluator provider")
	}
	if env == nil {
		return nil, errors.New("evaluator environment provider is required")
	}

	baseURL := cmp.Or(cfg.BaseURL, defaultURL)
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = strings.TrimRight(baseURL, "/")
		if cfg.Provider == "openai" && strings.HasSuffix(endpoint, "/v1") {
			endpoint += "/decisions"
		} else {
			endpoint += path
		}
	}

	if cfg.Endpoint != "" {
		u, _ := url.Parse(endpoint)
		baseURL = u.Scheme + "://" + u.Host
		if cfg.Provider == "openai" && endpoint == openAIBaseURL+"/decisions" {
			baseURL = openAIBaseURL
		}
	}

	modelOpts := options.ForEvaluator(cfg, opts...)
	if gateway := modelOpts.Gateway(); gateway != "" {
		u, err := url.Parse(gateway)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
			return nil, errors.New("evaluator gateway must be an HTTP(S) URL without credentials or fragment")
		}
		if err := base.VerifyDockerGatewayAuth(ctx, env, gateway); err != nil {
			return nil, err
		}
	}

	var cost *latest.CostConfig
	if cfg.Cost != nil {
		price := *cfg.Cost
		cost = &price
	}
	client := httpclient.NewHTTPClient(ctx)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if modelOpts.Gateway() == "" {
		modelOpts.WrapTransport(ctx, client)
	}
	connection := evaluatorClient{
		client: client, env: env, endpoint: endpoint,
		tokenKey: cmp.Or(cfg.TokenKey, tokenKey), model: cfg.Model,
		timeout: cfg.Timeout.Duration, cost: cost,
		officialPricing: endpoint == openAIBaseURL+"/decisions",
		provider:        cfg.Provider, baseURL: baseURL, modelOpts: modelOpts,
	}
	// TypeSafe's base URL has no version suffix.
	if cfg.Provider == "typesafe" {
		connection.officialPricing = endpoint == defaultBaseURL+path
	}
	if connection.timeout == 0 {
		connection.timeout = defaultTimeout
	}
	result := assessment{resultType: cfg.Type, questionType: cfg.Type}
	switch cfg.Type {
	case "boolean":
		result.questionType = "noul"
	case "choice":
		result.probabilityKeys = slices.Sorted(maps.Keys(cfg.Choices))
	case "score":
		for i := range cfg.Levels {
			result.probabilityKeys = append(result.probabilityKeys, strconv.Itoa(i))
		}
	}
	if cfg.Provider == "openai" {
		return newOpenAI(connection, result, cfg)
	}
	question := typesafeQuestion{Type: result.questionType, Instructions: cfg.Instructions}
	switch cfg.Type {
	case "choice":
		question.Criteria = cfg.Choices
	case "score":
		question.Criteria = cfg.Levels
	}
	questionJSON, err := json.Marshal(question)
	if err != nil {
		return nil, errors.New("invalid evaluator question")
	}
	return &typesafe{evaluatorClient: connection, assessment: result, question: questionJSON}, nil
}
