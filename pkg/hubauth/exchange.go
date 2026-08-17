package hubauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/version"
)

const (
	// defaultLoginEndpoint is Docker Hub's token exchange endpoint: the same
	// one `docker login` uses.
	defaultLoginEndpoint = "https://hub.docker.com/v2/users/login"

	// envLoginURL overrides the exchange endpoint (for staging). Restricted to
	// HTTPS Docker hosts so it cannot be used to harvest the PAT.
	envLoginURL = "DOCKER_AGENT_HUB_LOGIN_URL"

	// envNoExchange opts out of minting entirely, for users who would rather
	// docker-agent didn't use their stored access token.
	envNoExchange = "DOCKER_AGENT_NO_TOKEN_EXCHANGE"

	// expectedAudience and trustedIssuer are the claims a token must carry to
	// be worth caching: a response that isn't a Docker-issued Hub token means
	// we're not talking to Docker.
	expectedAudience = "https://hub.docker.com"

	// maxAttempts bounds how often a single mint retries a transient failure.
	maxAttempts = 3

	// maxRetryAfter is the longest server-requested delay we sit through; past
	// that we give up and let the caller's cooldown handle it.
	maxRetryAfter = 3 * time.Second
)

// trustedIssuers are the Docker services that issue tokens for Hub.
var trustedIssuers = []string{"https://api.docker.com/", "https://login.docker.com/"}

// errRejected means Docker refused the stored access token: it was revoked,
// or it never had access. Retrying won't help until the user signs in again.
var errRejected = errors.New("the stored access token was refused, sign in again with `docker login`")

// errTransient marks a failure worth retrying (network trouble, rate limits,
// server errors).
var errTransient = errors.New("temporary failure")

// Overridable for tests, which must neither read the developer's credential
// store nor reach the real Hub.
var (
	lookupCredentials = dockerConfigCredentials
	httpClient        = &http.Client{
		// A redirect would resend the PAT to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
)

// loginEndpoint resolves the exchange endpoint once per process.
var loginEndpoint = sync.OnceValue(resolveLoginEndpoint)

func resolveLoginEndpoint() string {
	override := os.Getenv(envLoginURL)
	if override == "" {
		return defaultLoginEndpoint
	}
	if u, err := url.Parse(override); err != nil || u.Scheme != "https" || !isDockerHost(u.Hostname()) {
		slog.Warn("Ignoring "+envLoginURL+": not an HTTPS docker.com URL", "url", override)
		return defaultLoginEndpoint
	}
	return override
}

func isDockerHost(host string) bool {
	return host == "docker.com" || strings.HasSuffix(host, ".docker.com")
}

func exchangeDisabled() bool {
	switch strings.ToLower(os.Getenv(envNoExchange)) {
	case "", "0", "false":
		return false
	default:
		return true
	}
}

// exchangeWithRetry trades the PAT for a token, retrying transient failures
// within the caller's budget.
func exchangeWithRetry(ctx context.Context, username, secret string) (string, error) {
	var err error
	for attempt := 1; ; attempt++ {
		var token string
		var retryAfter time.Duration
		token, retryAfter, err = exchange(ctx, username, secret)
		if err == nil {
			return token, nil
		}
		if !errors.Is(err, errTransient) || attempt == maxAttempts {
			return "", err
		}

		delay := retryDelay(attempt, retryAfter)
		if delay == 0 {
			return "", err
		}
		slog.DebugContext(ctx, "Retrying the Docker token exchange", "in", delay, "error", err)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", errors.Join(err, ctx.Err())
		}
	}
}

// retryDelay returns how long to wait before the next attempt, or 0 to give up
// (a server asking for more than [maxRetryAfter] wants us gone).
func retryDelay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > maxRetryAfter {
			return 0
		}
		return retryAfter
	}
	// Exponential with jitter, so concurrent processes don't retry in lockstep.
	base := time.Duration(1<<attempt) * 100 * time.Millisecond
	return base + rand.N(base/2)
}

// exchange performs one token exchange. It returns the server's requested
// delay alongside the error when the failure is worth retrying.
func exchange(ctx context.Context, username, secret string) (token string, retryAfter time.Duration, err error) {
	body, err := json.Marshal(map[string]string{"username": username, "password": secret})
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginEndpoint(), bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", version.AppName+"/"+version.Version)

	resp, err := httpClient.Do(req)
	if err != nil {
		// Timeouts, DNS and connection failures are all worth another try.
		return "", 0, fmt.Errorf("%w: exchanging access token: %w", errTransient, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", retryAfterFrom(resp.Header), statusError(resp.StatusCode)
	}

	// Hub's clock is authoritative for the tokens it issues, and only a token
	// response proves we reached Hub: an error page from a TLS-terminating
	// proxy must not shift this process's idea of the time.
	learnClockSkew(resp.Header)

	var parsed struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed); err != nil {
		return "", 0, fmt.Errorf("decoding token exchange response: %w", err)
	}
	if err := validate(parsed.Token); err != nil {
		return "", 0, err
	}
	return parsed.Token, 0, nil
}

// statusError classifies an unsuccessful exchange.
func statusError(status int) error {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return fmt.Errorf("%w (HTTP %d)", errRejected, status)
	case status == http.StatusTooManyRequests, status >= 500:
		return fmt.Errorf("%w: exchanging access token: HTTP %d", errTransient, status)
	default:
		return fmt.Errorf("exchanging access token: HTTP %d", status)
	}
}

// retryAfterFrom reads the Retry-After header, in either of its two forms. The
// date form is resolved against the response's own Date header: that date is
// expressed in the sender's clock, which this response may be the first thing
// to tell us about.
func retryAfterFrom(header http.Header) time.Duration {
	value := header.Get("Retry-After")
	if value == "" {
		return 0
	}
	if seconds, err := time.ParseDuration(value + "s"); err == nil {
		return max(seconds, 0)
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(date.Sub(sentAt(header)), 0)
	}
	return 0
}

// sentAt returns when the response was sent, as its sender saw it, falling back
// to our own corrected clock when it said nothing.
func sentAt(header http.Header) time.Time {
	if date, err := http.ParseTime(header.Get("Date")); err == nil {
		return date
	}
	return now()
}

// validate rejects an exchange result we shouldn't cache: an empty token, one
// that isn't a Docker-issued Hub token, or one that is already due for renewal
// (accepting it would exchange the PAT again on the very next call).
func validate(token string) error {
	if token == "" {
		return errors.New("token exchange returned no token")
	}
	claims, err := parseClaims(token)
	if err != nil {
		return fmt.Errorf("token exchange returned an unreadable token: %w", err)
	}
	issuer, _ := claims.GetIssuer()
	if !slices.Contains(trustedIssuers, issuer) {
		return fmt.Errorf("token exchange returned a token from an unexpected issuer %q", issuer)
	}
	audience, _ := claims.GetAudience()
	if !slices.Contains(audience, expectedAudience) {
		return fmt.Errorf("token exchange returned a token for an unexpected audience %q", audience)
	}
	if !now().Before(renewAt(token)) {
		return errors.New("token exchange returned a token too close to expiry")
	}
	return nil
}
