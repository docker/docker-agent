package provider

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/base"
)

// Option customizes how evaluator clients connect.
type Option func(*options)

type options struct {
	gateway          string
	transportWrapper func(http.RoundTripper) http.RoundTripper
}

// WithModelsGateway routes evaluators that don't use a custom URL or an explicit
// bypass through gateway, authenticating like LLM providers do.
func WithModelsGateway(gateway string) Option {
	return func(o *options) { o.gateway = gateway }
}

// WithHTTPTransportWrapper wraps the transport of every evaluator client, like
// the model option of the same name. A nil wrapper is ignored.
func WithHTTPTransportWrapper(wrap func(http.RoundTripper) http.RoundTripper) Option {
	return func(o *options) { o.transportWrapper = wrap }
}

// backend describes how a provider is addressed directly and through a gateway.
type backend struct {
	name string
	// tokenKey is the default credential variable in direct mode.
	tokenKey string
	// defaultBaseURL is the public API base. The gateway forwards to it.
	defaultBaseURL string
	// path is appended to the API base in direct mode.
	path string
	// gatewayPath is appended to the gateway URL.
	gatewayPath string
}

var (
	typesafeBackend = backend{name: latest.EvaluatorProviderTypeSafe, tokenKey: "TYPESAFE_API_KEY", defaultBaseURL: defaultBaseURL, path: "/v1/systemone", gatewayPath: "/v1/systemone"}
	openaiBackend   = backend{name: latest.EvaluatorProviderOpenAI, tokenKey: "OPENAI_API_KEY", defaultBaseURL: "https://api.openai.com/v1", path: "/decisions", gatewayPath: "/v1/decisions"}
)

// connection sends evaluator requests directly or through a models gateway.
// Credentials are resolved on every request.
type connection struct {
	client   *http.Client
	env      environment.Provider
	endpoint string
	tokenKey string
	// gateway is set in gateway mode, where tokenKey is unused.
	gateway string
	// refresh replaces a token the gateway rejected; nil disables the single replay.
	refresh func(ctx context.Context, rejected string) (string, error)
}

func newConnection(ctx context.Context, cfg latest.EvaluatorConfig, env environment.Provider, b backend, opts []Option) (*connection, error) {
	var o options
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}

	c := &connection{env: env, tokenKey: cmp.Or(cfg.TokenKey, b.tokenKey)}
	var httpOpts []httpclient.Opt
	if cfg.UsesModelsGateway(o.gateway) {
		endpoint, gatewayURL, err := gatewayEndpoint(o.gateway, b.gatewayPath)
		if err != nil {
			return nil, err
		}
		c.endpoint, c.gateway = endpoint, o.gateway
		c.refresh = base.GatewayAuthRefresh(env, o.gateway)
		httpOpts = base.GatewayHTTPOptions(gatewayURL, b.defaultBaseURL, &latest.ModelConfig{Provider: b.name, Model: cfg.Model}, nil)
	} else {
		endpoint, err := directEndpoint(cfg, b)
		if err != nil {
			return nil, err
		}
		c.endpoint = endpoint
	}

	c.client = httpclient.NewHTTPClient(ctx, httpOpts...)
	if o.transportWrapper != nil {
		if wrapped := o.transportWrapper(c.client.Transport); wrapped != nil {
			c.client.Transport = wrapped
		}
	}
	c.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c, nil
}

func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

func directEndpoint(cfg latest.EvaluatorConfig, b backend) (string, error) {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = b.defaultBaseURL
	}
	if !validHTTPURL(baseURL) {
		return "", errors.New("evaluator base URL must be an HTTP(S) URL without credentials, query, or fragment")
	}
	if cfg.Endpoint == "" {
		return strings.TrimRight(baseURL, "/") + b.path, nil
	}
	if !validHTTPURL(cfg.Endpoint) {
		return "", errors.New("evaluator endpoint must be an HTTP(S) URL without credentials, query, or fragment")
	}
	return cfg.Endpoint, nil
}

// gatewayEndpoint places the native API path directly beneath the gateway's path
// prefix. The gateway's query is added per request by the HTTP client.
func gatewayEndpoint(gateway, suffix string) (string, *url.URL, error) {
	u, err := url.Parse(gateway)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", nil, errors.New("evaluator models gateway must be an HTTP(S) URL without credentials or fragment")
	}
	if hasDotSegment(u.Path) {
		return "", nil, errors.New("evaluator models gateway path must not contain dot segments")
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/") + suffix, u, nil
}

// hasDotSegment also checks a second decoding level, so %252e%252e can't alias a parent path.
func hasDotSegment(path string) bool {
	for range 2 {
		for segment := range strings.FieldsFuncSeq(path, func(r rune) bool { return r == '/' || r == '\\' }) {
			if segment == "." || segment == ".." {
				return true
			}
		}
		decoded, err := url.PathUnescape(path)
		if err != nil || decoded == path {
			return false
		}
		path = decoded
	}
	return false
}

// token returns the credential for one request.
func (c *connection) token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.gateway != "" {
		token, err := base.GatewayAuthToken(ctx, c.env, c.gateway)
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err != nil {
			return "", fmt.Errorf("evaluator gateway authentication failed: %w", err)
		}
		return token, nil
	}
	token, ok := c.env.Get(ctx, c.tokenKey)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !ok || strings.TrimSpace(token) == "" {
		return "", errors.New("evaluator API key is missing")
	}
	return token, nil
}

// exchange is the outcome of a request. Status and body are valid when err is nil.
type exchange struct {
	status int
	body   []byte
	// tooLarge and readFailed mean body is unusable.
	tooLarge, readFailed bool
	// attempted reports that the last HTTP request was made, even if it failed,
	// so the caller must account for it.
	attempted bool
}

// usable reports whether body is complete enough to decode.
func (x exchange) usable() bool { return !x.tooLarge && !x.readFailed }

// send posts payload. A trusted gateway that rejects the token gets one replay with
// a fresh one; the rejected attempt is reported to the usage observer here, and
// the final attempt is left to the caller.
func (c *connection) send(ctx context.Context, payload []byte, model string) (exchange, error) {
	token, err := c.token(ctx)
	if err != nil {
		return exchange{}, err
	}
	for replayed := false; ; replayed = true {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
		if err != nil {
			return exchange{}, errors.New("failed to construct evaluator request")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, err := c.client.Do(req)
		if err != nil {
			return exchange{attempted: true}, requestError(ctx, "evaluator request failed")
		}
		x := exchange{status: resp.StatusCode, attempted: true}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		resp.Body.Close()
		switch {
		case readErr != nil:
			x.readFailed = true
		case len(body) > maxResponseBytes:
			x.tooLarge = true
		default:
			x.body = body
		}

		if x.status != http.StatusUnauthorized || replayed || c.refresh == nil || token == "" {
			return x, nil
		}
		fresh, err := c.refresh(ctx, token)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return x, requestError(ctx, "evaluator request failed")
		}
		if err != nil || fresh == "" || fresh == token {
			return x, nil
		}
		evaluator.ObserveUsage(ctx, evaluator.UsageRecord{Model: model})
		token = fresh
	}
}

// failure describes an unusable exchange without echoing the response.
func (c *connection) failure(ctx context.Context, x exchange) error {
	switch {
	case x.status != http.StatusOK:
		return fmt.Errorf("evaluator returned HTTP status %d", x.status)
	case x.readFailed:
		return requestError(ctx, "failed to read evaluator response")
	case x.tooLarge:
		return errors.New("evaluator response exceeds size limit")
	}
	return nil
}

// Preserve cancellation identity without exposing transport errors or URLs.
func requestError(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", message, err)
	}
	return errors.New(message)
}
