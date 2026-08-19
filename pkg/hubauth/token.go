// Package hubauth mints Docker access tokens from the personal access token
// that `docker login` (including Docker Desktop's sign-in) leaves in the
// Docker CLI credential store.
//
// Docker Desktop's backend API only ever hands out its own access token —
// valid for 15 minutes — and never the refresh token behind it, so callers
// cannot renew it: when Desktop's background refresher is stuck, every caller
// keeps getting the same expired JWT. The stored PAT is long-lived and Docker
// Hub exchanges it for a fresh token without any user interaction, which gives
// docker-agent a token source it controls.
//
// The PAT never leaves this process except in the exchange request to Docker
// Hub: the endpoint is pinned to a Docker host, redirects are not followed,
// and account passwords are never sent.
package hubauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	// renewBefore is how long before its expiry a minted token is replaced,
	// so callers never receive one that dies mid-request.
	renewBefore = time.Minute

	// unknownExpiryTTL bounds the reuse of a minted token whose exp claim we
	// can't read, instead of exchanging the PAT on every call.
	unknownExpiryTTL = 5 * time.Minute

	// credCheckTTL is how long a minted token is served before the credential
	// store is consulted again, so a `docker logout` or an account switch is
	// picked up quickly without shelling out to a credential helper on every
	// call.
	credCheckTTL = 30 * time.Second

	// mintBudget bounds the exchange with Hub, retries included. It cannot
	// interrupt a hung credential helper (those take no context), but callers
	// are never blocked on one: they wait on their own context.
	mintBudget = 15 * time.Second

	// failureCooldown keeps a broken credential store or an unreachable Hub
	// from adding latency to every single call.
	failureCooldown = 30 * time.Second

	// rejectedCooldown applies when Docker refuses the stored token: that
	// won't fix itself, so back off far longer than for a transient failure.
	rejectedCooldown = 5 * time.Minute
)

// errNoCredentials means the credential store holds no access token we can
// exchange, so any token minted earlier no longer represents the user.
var errNoCredentials = errors.New("no Docker access token in the credential store")

var state struct {
	sync.Mutex

	token         string
	renewAt       time.Time     // time from which the token must be replaced
	credHash      string        // fingerprint of the credentials that minted it
	credCheckedAt time.Time     // last time those credentials were confirmed
	lastErr       error         // why the last attempt failed
	nextAttempt   time.Time     // earliest time a new attempt may start
	inflight      chan struct{} // closed when the in-flight attempt completes
}

// Token returns a Docker token minted from the stored PAT, reusing the last one
// until it is about to expire. Callers get an error when no PAT is available
// (not signed in, or signed in with a password) or when Hub refuses the
// exchange.
//
// Attempts are singleflighted and run detached from the caller — reading the
// credential store shells out to a helper that ignores cancellation, and the
// result serves everyone — so a caller whose context is canceled returns
// immediately without holding up the others.
func Token(ctx context.Context) (string, error) {
	if exchangeDisabled() {
		return "", errors.New("token exchange is disabled by " + envNoExchange)
	}

	state.Lock()

	current := now()
	if state.token != "" && current.Before(state.renewAt) && current.Before(state.credCheckedAt.Add(credCheckTTL)) {
		token := state.token
		state.Unlock()
		return token, nil
	}

	if inflight := state.inflight; inflight != nil {
		state.Unlock()
		return await(ctx, inflight)
	}

	if current.Before(state.nextAttempt) {
		// A usable token from before the failure beats no token at all.
		if state.token != "" && !Expiring(state.token) {
			token := state.token
			state.Unlock()
			return token, nil
		}
		wait, err := state.nextAttempt.Sub(current).Round(time.Second), state.lastErr
		state.Unlock()
		return "", fmt.Errorf("waiting %s before retrying: %w", wait, err)
	}

	done := make(chan struct{})
	state.inflight = done
	state.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mintBudget)
		defer cancel()

		token, credHash, err := freshToken(ctx)

		state.Lock()
		defer state.Unlock()
		record(token, credHash, err)
		state.inflight = nil
		close(done)
	}()

	return await(ctx, done)
}

// Invalidate drops token from the cache, so the next [Token] call mints a new
// one. Called when Docker rejects a token we believed to be valid; a token
// that has since been replaced is left alone.
func Invalidate(token string) {
	if token == "" {
		return
	}

	state.Lock()
	defer state.Unlock()
	if state.token != token {
		return
	}
	state.token, state.credHash, state.renewAt = "", "", time.Time{}
	forget()
}

// await waits for the in-flight attempt, or gives up when the caller's own
// context is canceled.
func await(ctx context.Context, done <-chan struct{}) (string, error) {
	select {
	case <-done:
		state.Lock()
		defer state.Unlock()
		if state.token != "" {
			return state.token, nil
		}
		return "", state.lastErr
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// record stores the outcome of an attempt. It must be called with the state
// lock held. A token that is still usable survives a failed renewal, unless
// the credentials behind it are gone or refused: it then no longer represents
// the user.
func record(token, credHash string, err error) {
	if err != nil {
		state.lastErr = err
		state.nextAttempt = now().Add(cooldownFor(err))
		if errors.Is(err, errNoCredentials) || errors.Is(err, errRejected) || Expiring(state.token) {
			state.token, state.credHash = "", ""
			forget()
		}
		return
	}

	state.token = token
	state.credHash = credHash
	state.renewAt = renewAt(token)
	state.credCheckedAt = now()
	state.lastErr = nil
	state.nextAttempt = time.Time{}
}

func cooldownFor(err error) time.Duration {
	if errors.Is(err, errRejected) {
		return rejectedCooldown
	}
	return failureCooldown
}

// freshToken returns a token minted from the credentials currently in the
// store, along with their fingerprint. The cached token is kept when it comes
// from those same credentials and is not due for renewal.
func freshToken(ctx context.Context) (token, credHash string, err error) {
	username, secret, err := lookupCredentials()
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", errNoCredentials, err)
	}
	if username == "" || !isAccessToken(secret) {
		return "", "", errNoCredentials
	}
	credHash = fingerprint(username, secret)

	state.Lock()
	cached, sameCredentials, dueForRenewal := state.token, credHash == state.credHash, !now().Before(state.renewAt)
	state.Unlock()

	if cached != "" && sameCredentials && !dueForRenewal {
		return cached, credHash, nil
	}

	// A token minted by another docker-agent process is as good as ours.
	if shared, ok := load(credHash); ok {
		slog.DebugContext(ctx, "Reusing a Docker token minted by another process")
		return shared, credHash, nil
	}

	token, err = exchangeWithRetry(ctx, username, secret)
	if err != nil {
		return "", credHash, err
	}
	store(credHash, token)
	return token, credHash, nil
}
