package acp

import (
	"context"
	"errors"
	"maps"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

const hostCredentialsMethod = "host-credentials"

func hostCredentialMethods() []acp.AuthMethod {
	description := "Use credentials configured on the agent host. Configure keys or sign in outside ACP, then retry. This checks configuration availability, not remote token validity."
	return []acp.AuthMethod{{Agent: &acp.AuthMethodAgent{Id: hostCredentialsMethod, Name: "Use configured host credentials", Description: &description}}}
}

func hostCredentialsRequired() *acp.RequestError {
	return acp.NewAuthRequired(map[string]any{"methodId": hostCredentialsMethod, "message": "Configure credentials on the agent host, then authenticate with host-credentials."})
}

func isAuthenticationRequired(err error) bool {
	if errors.Is(err, config.ErrGatewayAuthentication) {
		return true
	}
	if required, ok := errors.AsType[*environment.RequiredEnvError](err); ok {
		return required.MissingModelCredentials
	}
	var modelCredentials interface{ MissingModelCredentials() bool }
	return errors.As(err, &modelCredentials) && modelCredentials.MissingModelCredentials()
}

// authenticationErrorLocked guards connection access, not shared host secrets.
func (a *Agent) authenticationErrorLocked() error {
	if a.authBlocked || a.logout != nil || a.authenticating != nil {
		return hostCredentialsRequired()
	}
	return nil
}

type authRuntimeConfigKey struct{}

func (a *Agent) runtimeConfig(ctx context.Context) *config.RuntimeConfig {
	if rc, ok := ctx.Value(authRuntimeConfigKey{}).(*config.RuntimeConfig); ok {
		return rc
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.authRunConfig != nil {
		return a.authRunConfig
	}
	if a.runConfig != nil {
		return a.runConfig
	}
	return &config.RuntimeConfig{}
}

// Authenticate adopts configured credentials; it never collects or writes them.
func (a *Agent) Authenticate(ctx context.Context, params acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	ctx, span := startACPRequest(ctx, "authenticate", params.Meta)
	defer span.End()
	empty := acp.AuthenticateResponse{}
	a.mu.Lock()
	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return empty, err
	}
	if a.stopped || !a.initialized {
		a.mu.Unlock()
		return empty, acp.NewInvalidRequest("agent is stopped or not initialized")
	}
	if params.MethodId != hostCredentialsMethod {
		a.mu.Unlock()
		return empty, acp.NewInvalidParams("unknown authentication method")
	}
	if a.authenticating != nil {
		a.mu.Unlock()
		return empty, acp.NewInvalidRequest("authentication in progress")
	}
	if logout := a.logout; logout != nil {
		select {
		case <-logout.done:
			if logout.err != nil {
				a.mu.Unlock()
				return empty, acp.NewInternalError("connection cleanup failed")
			}
		default:
			a.mu.Unlock()
			return empty, acp.NewInvalidRequest("logout cleanup in progress")
		}
	}
	if a.cleanupErr != nil {
		a.mu.Unlock()
		return empty, acp.NewInternalError("connection cleanup failed")
	}
	if !a.authBlocked && !a.credentialRefresh {
		a.mu.Unlock()
		return empty, nil
	}
	if len(a.sessions) != 0 || len(a.owned) != 0 {
		a.mu.Unlock()
		return empty, acp.NewInvalidRequest("close loaded sessions before refreshing credentials")
	}
	for pending := range a.pending {
		if pending.lifecycle != nil {
			a.mu.Unlock()
			return empty, acp.NewInvalidRequest("session operation in progress")
		}
	}
	for _, lifecycle := range a.lifecycles {
		if lifecycle.err != nil {
			a.mu.Unlock()
			return empty, acp.NewInternalError("connection cleanup failed")
		}
		if lifecycle.closing {
			select {
			case <-lifecycle.done:
			default:
				a.mu.Unlock()
				return empty, acp.NewInvalidRequest("session cleanup in progress")
			}
		}
	}
	a.authBlocked = true
	a.logout = nil
	ctx, op := a.startOperationLocked(ctx, nil)
	a.authenticating = op
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.authenticating = nil
		a.mu.Unlock()
		a.finishOperation(op)
	}()
	rc := a.runtimeConfig(ctx).CloneWithFreshEnvironment()
	if err := rc.EnvFilesError(); err != nil {
		return empty, acp.NewInternalError("could not read configured credential files")
	}
	ctx = context.WithValue(ctx, authRuntimeConfigKey{}, rc)
	loaded, err := a.loadTeamSerialized(ctx, a.defaultWorkingDir())
	if err != nil {
		if ctx.Err() != nil {
			return empty, ctx.Err()
		}
		if isAuthenticationRequired(err) {
			return empty, hostCredentialsRequired()
		}
		return empty, acp.NewInternalError("could not load agent configuration with configured credentials")
	}
	a.mu.Lock()
	previous := a.team
	a.team = nil
	a.mu.Unlock()
	if previous != nil {
		if err := a.discardSession(ctx, op, &Session{team: previous}); err != nil {
			_ = a.discardSession(ctx, op, &Session{team: loaded.Team})
			return empty, acp.NewInternalError("connection cleanup failed")
		}
	}
	a.mu.Lock()
	if a.stopped || a.logout != nil || ctx.Err() != nil {
		a.mu.Unlock()
		_ = a.discardSession(ctx, op, &Session{team: loaded.Team})
		cause := ctx.Err()
		if cause == nil {
			cause = context.Canceled
		}
		return empty, cause
	}
	a.team, a.authRunConfig, a.authBlocked, a.credentialRefresh = loaded.Team, rc, false, false
	a.mu.Unlock()
	return empty, nil
}

type agentLogout struct {
	done chan struct{}
	err  error
}

// Logout revokes this connection's access and drains owned work, not host logins.
func (a *Agent) Logout(ctx context.Context, params acp.LogoutRequest) (acp.LogoutResponse, error) {
	ctx, span := startACPRequest(ctx, "logout", params.Meta)
	defer span.End()
	empty := acp.LogoutResponse{}
	a.mu.Lock()
	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return empty, err
	}
	if a.stopped || !a.initialized {
		a.mu.Unlock()
		return empty, acp.NewInvalidRequest("agent is stopped or not initialized")
	}
	logout := a.logout
	if logout == nil {
		logout = &agentLogout{done: make(chan struct{})}
		a.logout, a.authBlocked = logout, true
		var pending []<-chan struct{}
		for op := range a.pending {
			op.cancel()
			pending = append(pending, op.done)
		}
		var closing []*sessionLifecycle
		for sid := range a.sessions {
			a.closeSessionLocked(ctx, sid)
		}
		for sid := range a.lifecycles {
			closing = append(closing, a.closeSessionLocked(ctx, sid))
		}
		validationTeam := a.team
		a.team = nil
		drainCtx, op := a.startOperationLocked(context.WithoutCancel(ctx), nil)
		a.mu.Unlock()
		go func() {
			defer a.finishOperation(op)
			for _, done := range pending {
				<-done
			}
			var errs []error
			for _, lifecycle := range closing {
				<-lifecycle.done
				errs = append(errs, lifecycle.err)
			}
			if validationTeam != nil {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(drainCtx), 30*time.Second)
				errs = append(errs, validationTeam.StopToolSets(cleanupCtx))
				cancel()
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			logout.err = errors.Join(append(errs, a.cleanupErr)...)
			a.cleanupErr = errors.Join(a.cleanupErr, logout.err)
			a.authRunConfig = nil
			close(logout.done)
		}()
	} else {
		a.mu.Unlock()
	}
	if err := waitForCleanup(ctx, logout.done); err != nil {
		return empty, err
	}
	if logout.err != nil {
		return empty, acp.NewInternalError("connection cleanup failed")
	}
	return empty, nil
}

// Provider constructors do not consistently type missing-key errors. On a
// failed override, check only its model requirements using the same env snapshot.
func (a *Agent) modelAuthenticationError(ctx context.Context, s *Session, ref string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg := &latest.Config{Models: maps.Clone(s.configModels), Providers: s.configProviders}
	if cfg.Models == nil {
		cfg.Models = make(map[string]latest.ModelConfig)
	}
	seen := make(map[string]bool)
	var add func(string) bool
	add = func(ref string) bool {
		if ref == "" {
			return true
		}
		if seen[ref] {
			return false
		}
		seen[ref] = true
		defer delete(seen, ref)
		model, found := cfg.Models[ref]
		if found && model.Provider == "" && strings.Contains(model.Model, ",") {
			ref = model.Model
			found = false
		}
		if !found && strings.Contains(ref, ",") {
			for part := range strings.SplitSeq(ref, ",") {
				if !add(strings.TrimSpace(part)) {
					return false
				}
			}
			return true
		}
		if !found {
			var err error
			model, err = latest.ParseModelRef(ref)
			if err != nil {
				return false
			}
			cfg.Models[ref] = model
		}
		cfg.Agents = append(cfg.Agents, latest.AgentConfig{Model: ref})
		return true
	}
	if !add(ref) {
		return nil
	}
	rc := a.runtimeConfig(ctx)
	err := config.CheckRequiredEnvVars(ctx, cfg, rc.ModelsGateway, rc.EnvProvider())
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if isAuthenticationRequired(err) {
		a.mu.Lock()
		a.credentialRefresh = true
		a.mu.Unlock()
		return hostCredentialsRequired()
	}
	return nil
}
