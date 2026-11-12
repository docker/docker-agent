package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

type evaluatorClient struct {
	client          *http.Client
	env             environment.Provider
	endpoint        string
	tokenKey        string
	model           string
	timeout         time.Duration
	cost            *latest.CostConfig
	officialPricing bool
	provider        string
	baseURL         string
	modelOpts       options.ModelOptions
}

func (p *evaluatorClient) connection(ctx context.Context) (*http.Client, string, string, error) {
	if gateway := p.modelOpts.Gateway(); gateway != "" {
		u, _ := url.Parse(p.endpoint)
		gatewayURL, _ := url.Parse(gateway)
		gatewayURL.RawPath = strings.TrimRight(gatewayURL.EscapedPath(), "/")
		gatewayURL.Path, _ = url.PathUnescape(gatewayURL.RawPath)
		cfg := &latest.ModelConfig{Provider: p.provider, Model: p.model, BaseURL: p.baseURL}
		connection, err := base.NewGatewayClient(ctx, p.env, gatewayURL.String(), p.baseURL, u.EscapedPath(), cfg, &p.modelOpts, httpclient.WithHeader("X-Cagent-Evaluator", "1"))
		if err != nil {
			return nil, "", "", requestError(ctx, "failed to authenticate evaluator gateway")
		}
		connection.HTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		return connection.HTTPClient, connection.BaseURL, connection.AuthToken, nil
	}
	if source := p.modelOpts.TokenSource(); source != nil {
		token, err := source(ctx)
		if err != nil {
			return nil, "", "", requestError(ctx, "failed to resolve evaluator credentials")
		}
		if err := ctx.Err(); err != nil {
			return nil, "", "", err
		}
		return p.client, p.endpoint, token, nil
	}
	token, ok := p.env.Get(ctx, p.tokenKey)
	if err := ctx.Err(); err != nil {
		return nil, "", "", err
	}
	if !ok || strings.TrimSpace(token) == "" {
		return nil, "", "", errors.New("evaluator API key is missing")
	}
	return p.client, p.endpoint, token, nil
}

func (p *evaluatorClient) evaluate(ctx context.Context, state any, encode func(json.RawMessage) ([]byte, error), result func(json.RawMessage, evaluator.UsageRecord) (*evaluator.Result, error)) (*evaluator.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rawState, err := json.Marshal(state)
	if err != nil {
		return nil, errors.New("evaluator state must be JSON-serializable")
	}
	if len(rawState) == 0 || (rawState[0] != '"' && rawState[0] != '{' && rawState[0] != '[') {
		return nil, errors.New("evaluator state must be a string, object, or array")
	}
	payload, err := encode(rawState)
	if err != nil {
		return nil, errors.New("failed to encode evaluator request")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client, endpoint, token, err := p.connection(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("failed to construct evaluator request")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	record := evaluator.UsageRecord{Model: p.model}
	defer func() { evaluator.ObserveUsage(ctx, record) }()
	resp, err := client.Do(req)
	if err != nil {
		return nil, requestError(ctx, "evaluator request failed")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, requestError(ctx, "failed to read evaluator response")
	}
	if len(body) > maxResponseBytes {
		return nil, errors.New("evaluator response exceeds size limit")
	}
	var response typesafeResponse
	decodeErr := json.Unmarshal(body, &response)
	var model string
	modelErr := json.Unmarshal(response.Model, &model)
	if strings.TrimSpace(model) != "" {
		record.Model = model
	}
	var usageErr error
	if decodeErr == nil {
		if p.provider == "openai" {
			record.Usage, usageErr = reportedOpenAIUsage(response.Usage)
		} else {
			record.Usage, usageErr = reportedUsage(response.Usage)
		}
		record.Cost = p.estimateCost(model, record.Usage)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("evaluator returned HTTP status %d", resp.StatusCode)
	}
	if decodeErr != nil || (len(response.Model) != 0 && modelErr != nil) {
		return nil, errors.New("invalid evaluator response JSON")
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("evaluator response is missing the model")
	}
	if usageErr != nil {
		// Untrustworthy accounting must stop the run, not select a fallback.
		return nil, &evaluator.TerminalError{Err: usageErr}
	}
	return result(response.Answers, record)
}

// Preserve cancellation identity without exposing transport errors or URLs.
func requestError(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", message, err)
	}
	return errors.New(message)
}
