package acp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/coder/acp-go-sdk"
	"go.opentelemetry.io/otel"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/modelcatalog"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
	"github.com/docker/docker-agent/pkg/version"
)

// Agent implements the ACP Agent interface for docker agent.
type Agent struct {
	agentSource  config.Source
	runConfig    *config.RuntimeConfig
	sessionStore session.Store
	sessions     map[string]*Session

	conn              *acp.AgentSideConnection
	clientFS          acp.FileSystemCapabilities
	clientTerminal    bool
	clientElicitation acp.ElicitationCapabilities
	elicitationConn   *acp.AgentSideConnection
	team              *team.Team // Initialization validation only; sessions load their own teams.
	loadTeam          func(context.Context, string) (*teamloader.LoadResult, error)
	loadGate          chan struct{}

	mu                sync.Mutex
	stopped           bool
	initializing      bool
	initialized       bool
	credentialRefresh bool
	authBlocked       bool
	authenticating    *agentOperation
	logout            *agentLogout
	authRunConfig     *config.RuntimeConfig
	operations        sync.WaitGroup
	pending           map[*agentOperation]struct{}
	lifecycles        map[string]*sessionLifecycle
	stopDone          chan struct{}
	stopErr           error
	cleanupErr        error
	owned             map[*Session]struct{}
	deletion          *sessionDeletion
}

var _ acp.Agent = (*Agent)(nil)

// Session represents an ACP session.
type Session struct {
	id              string
	sess            *session.Session
	rt              runtime.Runtime
	team            *team.Team
	clientMCP       *clientMCPTools
	terminals       *terminalManager
	workingDir      string
	additionalDirs  []string
	usageAgent      string
	contextLimit    int64
	rootUsage       *runtime.Usage
	usageCosts      map[string]float64
	configModels    map[string]latest.ModelConfig
	configProviders map[string]latest.ProviderConfig
	configEnabled   bool
	lastConfig      *sessionConfiguration
	modelSelections map[string]modelSelection

	mu        sync.Mutex
	commandMu sync.Mutex

	turns       chan struct{}
	cancel      context.CancelFunc
	generation  uint64
	closed      bool
	loading     bool
	failed      error
	cleanupDone chan struct{}
	cleanupErr  error
}

var errSessionClosed = errors.New("ACP session closed")

func (s *Session) cancelTurn() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Session) close(ctx context.Context) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cleanupDone != nil {
		return s.cleanupDone
	}
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	s.initTurns()
	s.cleanupDone = make(chan struct{})
	var terminalsClosed chan error
	if s.terminals != nil {
		s.terminals.retire()
		terminalsClosed = make(chan error, 1)
		go func() { terminalsClosed <- s.terminals.cleanup(ctx) }()
	}
	var mcpClosed chan error
	if s.clientMCP != nil {
		generation := s.clientMCP.generation()
		if generation != nil {
			generation.retire()
			mcpClosed = make(chan error, 1)
			go func() { mcpClosed <- generation.close(ctx) }()
		}
	}
	go func() {
		defer close(s.cleanupDone)
		// The admitted turn drains runtime events before returning the token.
		<-s.turns
		defer func() { s.turns <- struct{}{} }()
		if s.rt != nil {
			if err := s.rt.Close(); err != nil {
				s.cleanupErr = errors.Join(s.cleanupErr, fmt.Errorf("closing ACP runtime: %w", err))
			}
		}
		if terminalsClosed != nil {
			s.cleanupErr = errors.Join(s.cleanupErr, <-terminalsClosed)
		}
		if mcpClosed != nil {
			s.cleanupErr = errors.Join(s.cleanupErr, <-mcpClosed)
		}
		if s.team != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if err := s.team.StopToolSets(cleanupCtx); err != nil {
				s.cleanupErr = errors.Join(s.cleanupErr, fmt.Errorf("stopping ACP toolsets: %w", err))
			}
		}
	}()
	return s.cleanupDone
}

// initTurns is called with s.mu held.
func (s *Session) initTurns() {
	if s.turns == nil {
		s.turns = make(chan struct{}, 1)
		s.turns <- struct{}{}
	}
}

func (s *Session) startTurn(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	turnCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return nil, nil, errSessionClosed
	}
	if s.loading {
		s.mu.Unlock()
		cancel()
		return nil, nil, acp.NewInvalidRequest("session is loading; retry after load completes")
	}
	if s.terminals != nil {
		if err := s.terminals.failure(); err != nil {
			s.mu.Unlock()
			cancel()
			return nil, nil, err
		}
	}
	if s.failed != nil {
		err := s.failed
		s.mu.Unlock()
		cancel()
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		cancel()
		return nil, nil, err
	}
	s.initTurns()
	turns := s.turns
	previous := s.cancel
	s.generation++
	generation := s.generation
	s.cancel = cancel
	s.mu.Unlock()

	if previous != nil {
		previous()
	}

	select {
	case <-turnCtx.Done():
		s.clearTurn(generation, cancel)
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return nil, nil, errSessionClosed
		}
		return nil, nil, turnCtx.Err()
	case <-turns:
	}

	s.mu.Lock()
	closed := s.closed
	failed := s.failed
	if s.terminals != nil {
		failed = errors.Join(failed, s.terminals.failure())
	}
	current := s.generation == generation
	err := turnCtx.Err()
	s.mu.Unlock()
	if closed || failed != nil || !current || err != nil {
		turns <- struct{}{}
		s.clearTurn(generation, cancel)
		if closed {
			return nil, nil, errSessionClosed
		}
		if failed != nil {
			return nil, nil, failed
		}
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, context.Canceled
	}

	finish := func() {
		turns <- struct{}{}
		s.clearTurn(generation, cancel)
	}
	return turnCtx, finish, nil
}

func (s *Session) clearTurn(generation uint64, cancel context.CancelFunc) {
	cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generation == generation {
		s.cancel = nil
	}
}

// NewAgent creates a new ACP agent.
func NewAgent(agentSource config.Source, runConfig *config.RuntimeConfig, sessionStore session.Store) *Agent {
	a := &Agent{
		agentSource:  agentSource,
		runConfig:    runConfig,
		sessionStore: sessionStore,
		sessions:     make(map[string]*Session),
		owned:        make(map[*Session]struct{}),
		loadGate:     make(chan struct{}, 1),
		pending:      make(map[*agentOperation]struct{}),
		lifecycles:   make(map[string]*sessionLifecycle),
	}
	a.loadTeam = func(ctx context.Context, workingDir string) (*teamloader.LoadResult, error) {
		opts := append(loaderdefaults.Opts(),
			teamloader.WithToolsetRegistry(createToolsetRegistry(a)),
			teamloader.WithWorkingDir(workingDir),
		)
		return teamloader.LoadWithConfig(ctx, a.agentSource, a.runtimeConfig(ctx), opts...)
	}
	return a
}

// SetAgentConnection sets the ACP connection.
func (a *Agent) SetAgentConnection(conn *acp.AgentSideConnection) {
	a.conn = conn
	a.elicitationConn = nil
}

// Initialize implements [acp.Agent].
func (a *Agent) Initialize(ctx context.Context, params acp.InitializeRequest) (acp.InitializeResponse, error) {
	ctx, span := startACPRequest(ctx, "initialize", params.Meta)
	defer span.End()
	slog.DebugContext(ctx, "ACP Initialize called", "client_version", params.ProtocolVersion)

	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return acp.InitializeResponse{}, errors.New("agent stopped")
	}
	if a.initialized || a.team != nil || a.initializing {
		a.mu.Unlock()
		return acp.InitializeResponse{}, errors.New("agent already initialized or initializing")
	}
	a.initializing = true
	ctx, op := a.startOperationLocked(ctx, nil)
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.initializing = false
		a.mu.Unlock()
		a.finishOperation(op)
	}()

	loadResult, err := a.loadTeamSerialized(ctx, a.defaultWorkingDir())
	if err != nil && !isAuthenticationRequired(err) {
		return acp.InitializeResponse{}, fmt.Errorf("failed to load teams: %w", err)
	}
	a.mu.Lock()
	if a.stopped || ctx.Err() != nil {
		a.mu.Unlock()
		var cleanupErr error
		if loadResult != nil {
			cleanupErr = a.discardSession(ctx, op, &Session{team: loadResult.Team})
		}
		return acp.InitializeResponse{}, errors.Join(context.Canceled, cleanupErr)
	}
	a.clientFS = params.ClientCapabilities.Fs
	a.clientTerminal = params.ClientCapabilities.Terminal
	if caps := params.ClientCapabilities.Elicitation; caps != nil {
		a.clientElicitation = *caps
	}
	a.initialized = true
	a.authBlocked = err != nil
	if loadResult != nil {
		a.team = loadResult.Team
	}
	a.mu.Unlock()

	agentTitle := "docker agent"
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AuthMethods:     hostCredentialMethods(),
		AgentInfo: &acp.Implementation{
			Name:    "docker agent",
			Version: version.Version,
			Title:   &agentTitle,
		},
		AgentCapabilities: acp.AgentCapabilities{
			Auth:        acp.AgentAuthCapabilities{Logout: &acp.LogoutCapabilities{}},
			LoadSession: true,
			SessionCapabilities: acp.SessionCapabilities{
				AdditionalDirectories: &acp.SessionAdditionalDirectoriesCapabilities{},
				Close:                 &acp.SessionCloseCapabilities{},
				Delete:                &acp.SessionDeleteCapabilities{},
				List:                  &acp.SessionListCapabilities{},
				Resume:                &acp.SessionResumeCapabilities{},
			},
			PromptCapabilities: acp.PromptCapabilities{
				EmbeddedContext: true,
				Image:           true,
				Audio:           true,
			},
			McpCapabilities: acp.McpCapabilities{
				Http: true,
				Sse:  true,
			},
		},
	}, nil
}

// newRuntime creates a session-owned team and runtime using the default agent.
func (a *Agent) newRuntime(ctx context.Context, sessionID, workingDir string, servers []clientMCPServer) (*Session, *agent.Agent, error) {
	workingDir = cmp.Or(workingDir, a.defaultWorkingDir())
	loadResult, err := a.loadTeamSerialized(ctx, workingDir)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if isAuthenticationRequired(err) {
			a.mu.Lock()
			a.credentialRefresh = true
			a.mu.Unlock()
			return nil, nil, hostCredentialsRequired()
		}
		return nil, nil, fmt.Errorf("failed to load session team: %w", err)
	}
	acpSess := &Session{team: loadResult.Team, clientMCP: &clientMCPTools{}}
	a.mu.Lock()
	if a.clientTerminal {
		acpSess.terminals = newTerminalManager(a.conn, sessionID)
	}
	a.mu.Unlock()
	for _, name := range acpSess.team.AgentNames() {
		agt, err := acpSess.team.Agent(name)
		if err != nil {
			return acpSess, nil, err
		}
		cfg, _ := acpSess.team.AgentConfig(name)
		agent.WithAdditionalToolSets(teamloader.WithReadOnlyFilter(acpSess.clientMCP, cfg.ReadOnly))(agt)
	}
	defaultAgent, err := acpSess.team.DefaultAgent()
	if err != nil {
		return acpSess, nil, fmt.Errorf("failed to resolve default agent: %w", err)
	}

	rc := a.runtimeConfig(ctx)
	acpSess.configEnabled = true
	acpSess.configModels = loadResult.Models
	acpSess.configProviders = loadResult.Providers
	switcher := &modelcatalog.Config{
		Models: loadResult.Models, Providers: loadResult.Providers, ProviderRegistry: loadResult.ProviderRegistry,
		AgentDefaultModels: loadResult.AgentDefaultModels, ModelsGateway: rc.ModelsGateway,
		EncryptedConfig: loadResult.EncryptedConfig, EnvProvider: rc.EnvProvider(),
	}
	if store, err := rc.ModelsDevStore(); err == nil {
		switcher.ModelsStore = store
	}
	handler := a.elicitationHandler(sessionID)
	rt, err := runtime.New(ctx, acpSess.team,
		runtime.WithCurrentAgent(defaultAgent.Name()),
		// Permission and iteration prompts retain their session-level interactivity.
		runtime.WithNonInteractive(handler == nil),
		runtime.WithElicitationHandler(handler),
		runtime.WithSessionStore(a.sessionStore),
		runtime.WithProviderRegistry(loadResult.ProviderRegistry),
		runtime.WithModelSwitcherConfig(switcher),
		runtime.WithModelsDevFallback(rc.ModelsDevFallback),
		runtime.WithWorkingDir(workingDir),
		// Match the CLI tracer scope so runtime spans remain enabled in ACP mode.
		runtime.WithTracer(otel.Tracer(version.AppName)),
	)
	if err != nil {
		return acpSess, nil, err
	}
	acpSess.rt = rt
	rt.OnBackgroundEvent(acpSess.retainUsage)
	generation, err := prepareClientMCP(ctx, servers, workingDir)
	acpSess.clientMCP.swap(generation)
	if err != nil {
		return acpSess, nil, err
	}
	return acpSess, defaultAgent, nil
}

// registerSessionIfAbsent transfers ownership only if this session wins registration.
func (a *Agent) registerSessionIfAbsent(ctx context.Context, acpSess *Session, lifecycle *sessionLifecycle) (*Session, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return nil, false, errors.New("agent stopped")
	}
	if err := a.authenticationErrorLocked(); err != nil {
		return nil, false, err
	}
	if a.lifecycles[acpSess.id] != lifecycle || lifecycle.closing {
		return nil, false, errSessionClosed
	}
	if lifecycle.err != nil {
		return nil, false, fmt.Errorf("session cleanup failed: %w", lifecycle.err)
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if existing, ok := a.sessions[acpSess.id]; ok {
		return existing, false, nil
	}
	a.sessions[acpSess.id] = acpSess
	a.owned[acpSess] = struct{}{}
	return acpSess, true, nil
}

// NewSession implements [acp.Agent].
func (a *Agent) NewSession(ctx context.Context, params acp.NewSessionRequest) (_ acp.NewSessionResponse, retErr error) {
	ctx, span := startACPRequest(ctx, "session/new", params.Meta)
	defer span.End()
	slog.DebugContext(ctx, "ACP NewSession called", "cwd", params.Cwd)

	servers, err := validateClientMCPServers(params.McpServers)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}

	workingDir, err := resolveWorkingDir(params.Cwd)
	if err != nil {
		return acp.NewSessionResponse{}, acp.NewInvalidParams(err.Error())
	}

	// Direct Go callers retain the legacy empty-cwd fallback; the SDK requires
	// cwd on the wire. Keep fallback execution paths out of saved provenance.
	if err := validateWorkingDir(workingDir); err != nil {
		return acp.NewSessionResponse{}, acp.NewInvalidParams(err.Error())
	}

	additionalDirs, err := resolveAdditionalDirectories(params.AdditionalDirectories)
	if err != nil {
		return acp.NewSessionResponse{}, acp.NewInvalidParams(err.Error())
	}

	sess := session.New(session.WithWorkingDir(workingDir))
	ctx, op, err := a.beginSessionConstruction(ctx, sess.ID)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	a.mu.Lock()
	op.lifecycle.newRoot = true
	a.mu.Unlock()
	defer a.finishOperation(op)
	acpSess, defaultAgent, err := a.newRuntime(ctx, sess.ID, workingDir, servers)
	stored := false
	defer func() {
		if !stored && acpSess != nil {
			if cleanupErr := a.discardSession(ctx, op, acpSess); cleanupErr != nil {
				retErr = errors.Join(retErr, cleanupErr)
			}
		}
	}()
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	if err := ctx.Err(); err != nil {
		return acp.NewSessionResponse{}, err
	}
	sess.MaxIterations = defaultAgent.MaxIterations()
	sess.MaxConsecutiveToolCalls = defaultAgent.MaxConsecutiveToolCalls()
	sess.MaxOldToolCallTokens = defaultAgent.MaxOldToolCallTokens()
	sess.MaxToolResultTokens = defaultAgent.MaxToolResultTokens()
	sess.SetTitle("ACP Session " + sess.ID)

	if err := a.sessionStore.AddSession(ctx, sess); err != nil {
		return acp.NewSessionResponse{}, fmt.Errorf("failed to persist session: %w", err)
	}

	slog.DebugContext(ctx, "ACP session created", "session_id", sess.ID)

	acpSess.id = sess.ID
	acpSess.sess = sess
	acpSess.workingDir = workingDir
	acpSess.additionalDirs = additionalDirs
	configuration := acpSess.configuration(ctx)
	_, stored, err = a.registerSessionIfAbsent(ctx, acpSess, op.lifecycle)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	if !stored {
		return acp.NewSessionResponse{}, session.ErrAlreadyExists
	}

	a.refreshCommands(ctx, acpSess)
	return acp.NewSessionResponse{SessionId: acp.SessionId(sess.ID), ConfigOptions: configuration.Options, Modes: configuration.Modes}, nil
}

// LoadSession implements [acp.AgentLoader].
func (a *Agent) LoadSession(ctx context.Context, params acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	ctx, span := startACPRequest(ctx, "session/load", params.Meta)
	defer span.End()
	var configuration sessionConfiguration
	err := a.reconnectSession(ctx, acp.ResumeSessionRequest{
		SessionId: params.SessionId, Cwd: params.Cwd,
		AdditionalDirectories: params.AdditionalDirectories, McpServers: params.McpServers,
	}, true, &configuration)
	return acp.LoadSessionResponse{ConfigOptions: configuration.Options, Modes: configuration.Modes}, err
}

// CloseSession implements [acp.Agent].
func (a *Agent) CloseSession(ctx context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	ctx, span := startACPRequest(ctx, "session/close", params.Meta)
	defer span.End()
	sid := string(params.SessionId)
	slog.DebugContext(ctx, "ACP CloseSession called", "session_id", sid)

	a.mu.Lock()
	lifecycle := a.closeSessionLocked(ctx, sid)
	a.mu.Unlock()
	if err := waitForCleanup(ctx, lifecycle.done); err != nil {
		return acp.CloseSessionResponse{}, err
	}
	return acp.CloseSessionResponse{}, lifecycle.err
}

// ResumeSession implements [acp.Agent].
func (a *Agent) ResumeSession(ctx context.Context, params acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	ctx, span := startACPRequest(ctx, "session/resume", params.Meta)
	defer span.End()
	var configuration sessionConfiguration
	err := a.reconnectSession(ctx, params, false, &configuration)
	return acp.ResumeSessionResponse{ConfigOptions: configuration.Options, Modes: configuration.Modes}, err
}

func (a *Agent) reconnectSession(ctx context.Context, params acp.ResumeSessionRequest, replay bool, configuration *sessionConfiguration) (retErr error) {
	sid := string(params.SessionId)
	slog.DebugContext(ctx, "ACP session reconnect", "session_id", sid, "replay", replay)
	if err := ctx.Err(); err != nil {
		return err
	}

	servers, err := validateClientMCPServers(params.McpServers)
	if err != nil {
		return err
	}
	workingDir, err := resolveWorkingDir(params.Cwd)
	if err != nil {
		return acp.NewInvalidParams(err.Error())
	}
	additionalDirs, err := resolveAdditionalDirectories(params.AdditionalDirectories)
	if err != nil {
		return acp.NewInvalidParams(err.Error())
	}

	ctx, op, err := a.beginSessionConstruction(ctx, sid)
	if err != nil {
		return err
	}
	defer a.finishOperation(op)

	a.mu.Lock()
	existing := a.sessions[sid]
	a.mu.Unlock()
	if existing != nil {
		return a.reconnectRegisteredSession(ctx, existing, workingDir, additionalDirs, servers, op, replay, configuration)
	}

	sess, err := a.sessionStore.GetSession(ctx, sid)
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			return sessionNotFound(sid)
		}
		return fmt.Errorf("failed to load session %s: %w", sid, err)
	}
	if err := validateResumeWorkingDir(sess.WorkingDir, workingDir); err != nil {
		return acp.NewInvalidParams(err.Error())
	}

	acpSess, _, err := a.newRuntime(ctx, sid, sess.WorkingDir, servers)
	stored := false
	defer func() {
		if !stored && acpSess != nil {
			if cleanupErr := a.discardSession(ctx, op, acpSess); cleanupErr != nil {
				retErr = errors.Join(retErr, cleanupErr)
			}
		}
	}()
	if err != nil {
		return err
	}

	overrides, _ := sess.ModelStateSnapshot()
	for name, ref := range overrides {
		if err := acpSess.rt.SetAgentModel(ctx, name, ref); err != nil {
			if authErr := a.modelAuthenticationError(ctx, acpSess, ref); authErr != nil {
				return authErr
			}
			return fmt.Errorf("restoring model override for %s: %w", name, err)
		}
		selected, _ := acpSess.team.Agent(name)
		value := "current"
		if _, ok := acpSess.configModels[ref]; ok {
			value = "model:" + ref
		}
		acpSess.rememberModelSelection(name, value, "", selected.SnapshotModelOverride())
	}
	acpSess.id = sid
	acpSess.sess = sess
	acpSess.workingDir = sess.WorkingDir
	acpSess.additionalDirs = additionalDirs
	var release func()
	if replay {
		acpSess.mu.Lock()
		acpSess.initTurns()
		<-acpSess.turns
		acpSess.loading = true
		acpSess.mu.Unlock()
		release = acpSess.finishLoading
		defer func() {
			if release != nil {
				release()
			}
		}()
	}
	*configuration = acpSess.configuration(ctx)
	existing, stored, err = a.registerSessionIfAbsent(ctx, acpSess, op.lifecycle)
	if err != nil {
		return err
	}
	if !stored {
		if release != nil {
			release()
			release = nil
		}
		cleanupErr := a.discardSession(ctx, op, acpSess)
		acpSess = nil
		if cleanupErr != nil {
			return cleanupErr
		}
		return a.reconnectRegisteredSession(ctx, existing, workingDir, additionalDirs, servers, op, replay, configuration)
	}

	slog.DebugContext(ctx, "ACP session resumed", "session_id", sid)
	if replay {
		return a.replayLoadedSession(ctx, acpSess)
	}
	a.refreshCommands(ctx, acpSess)
	return nil
}

// Cancel implements [acp.Agent].
func (a *Agent) Cancel(ctx context.Context, params acp.CancelNotification) error {
	ctx, span := startACPRequest(ctx, "session/cancel", params.Meta)
	defer span.End()
	sid := string(params.SessionId)
	slog.DebugContext(ctx, "ACP Cancel called", "session_id", sid)

	a.mu.Lock()
	acpSess, ok := a.sessions[sid]
	a.mu.Unlock()

	if ok && acpSess != nil {
		acpSess.cancelTurn()
	}

	return nil
}

// Prompt implements [acp.Agent].
func (a *Agent) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	ctx, span := startACPRequest(ctx, "session/prompt", params.Meta)
	defer span.End()
	sid := string(params.SessionId)
	slog.DebugContext(ctx, "ACP Prompt called", "session_id", sid)

	a.mu.Lock()
	if err := a.authenticationErrorLocked(); err != nil {
		a.mu.Unlock()
		return acp.PromptResponse{}, err
	}
	acpSess, ok := a.sessions[sid]
	a.mu.Unlock()

	if !ok {
		return acp.PromptResponse{}, sessionNotFound(sid)
	}

	turnCtx, finish, err := acpSess.startTurn(ctx)
	if err != nil {
		if errors.Is(err, errSessionClosed) {
			return acp.PromptResponse{}, sessionNotFound(sid)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		}
		return acp.PromptResponse{}, err
	}
	defer finish()
	turnCtx = withSessionID(turnCtx, sid)
	turnCtx = context.WithValue(turnCtx, terminalOwnerKey{}, acpSess.terminals)
	prompt, handled, err := a.dispatchCommand(turnCtx, acpSess, params.Prompt)
	if turnCtx.Err() != nil {
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	}
	if err != nil {
		return acp.PromptResponse{}, err
	}
	if handled {
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	userMsg := a.buildUserMessage(turnCtx, sid, prompt)
	if userMsg != nil && (userMsg.Message.Content != "" || len(userMsg.Message.MultiContent) > 0) {
		userMsg.Message.MessageID = uuid.NewV4().String()
		acpSess.sess.AddMessage(userMsg)
	}

	stopReason, err := a.runAgent(turnCtx, acpSess)
	if turnCtx.Err() != nil {
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	}
	if err != nil {
		return acp.PromptResponse{}, err
	}

	return acp.PromptResponse{StopReason: stopReason}, nil
}

// sendUpdate sends a session update notification to the ACP client.
func (a *Agent) sendUpdate(ctx context.Context, sessionID string, update acp.SessionUpdate) error {
	switch {
	case update.UserMessageChunk != nil && update.UserMessageChunk.MessageId == nil,
		update.AgentMessageChunk != nil && update.AgentMessageChunk.MessageId == nil,
		update.AgentThoughtChunk != nil && update.AgentThoughtChunk.MessageId == nil:
		update = withMessageID(update, uuid.NewV4().String())
	}
	return a.conn.SessionUpdate(ctx, acp.SessionNotification{
		Meta:      traceMeta(ctx, nil),
		SessionId: acp.SessionId(sessionID),
		Update:    update,
	})
}

// runAgent runs a single agent loop and streams updates to the ACP client.
func (a *Agent) runAgent(ctx context.Context, acpSess *Session) (stopReason acp.StopReason, retErr error) {
	slog.DebugContext(ctx, "Running agent turn", "session_id", acpSess.id)

	ctx = withSessionID(ctx, acpSess.id)
	ctx = context.WithValue(ctx, terminalOwnerKey{}, acpSess.terminals)

	if err := a.emitAvailableCommands(ctx, acpSess); err != nil {
		slog.DebugContext(ctx, "Failed to emit available commands", "error", err)
	}

	a.refreshConfiguration(ctx, acpSess)
	runCtx, cancel := context.WithCancel(ctx)
	eventsChan := acpSess.rt.RunStream(runCtx, acpSess.sess)
	var toolCalls toolCallTracker
	var messages liveMessages
	// Cancel before draining; final tool updates must precede the prompt response.
	defer func() {
		cancel()
		for event := range eventsChan {
			acpSess.retainUsage(event)
			toolCalls.retainResult(event)
		}
		cleanupCtx, stopCleanup := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer stopCleanup()
		updateErr := toolCalls.interrupt(cleanupCtx, a, acpSess)
		if ctx.Err() == nil {
			a.refreshConfiguration(ctx, acpSess)
		}
		if retErr == nil && ctx.Err() == nil {
			retErr = updateErr
		}
	}()
	outcome := promptOutcome{sessionID: acpSess.sess.ID}

	for event := range eventsChan {
		var usage *runtime.Usage
		if e, ok := event.(*runtime.TokenUsageEvent); ok {
			usage = acpSess.recordUsage(e)
		}
		if ctx.Err() != nil {
			toolCalls.retainResult(event)
			return "", ctx.Err()
		}

		outcome.observe(event)
		switch e := event.(type) {
		case *runtime.AgentChoiceEvent:
			if err := a.sendUpdate(ctx, acpSess.id, withMessageID(acp.UpdateAgentMessageText(e.Content), messages.id(e.SessionID, e.AgentName, e.MessageID, "content"))); err != nil {
				return "", err
			}

		case *runtime.AgentChoiceReasoningEvent:
			if err := a.sendUpdate(ctx, acpSess.id, withMessageID(acp.UpdateAgentThoughtText(e.Content), messages.id(e.SessionID, e.AgentName, e.MessageID, "thought"))); err != nil {
				return "", err
			}

		case *runtime.MessageAddedEvent:
			messages.finish(e.SessionID, e.AgentName)

		case *runtime.ToolCallConfirmationEvent:
			state, err := toolCalls.report(ctx, a, acpSess, e.AgentName, e.ToolCall, e.ToolDefinition, acp.ToolCallStatusPending)
			if err != nil {
				return "", err
			}
			rejected, err := a.handleToolCallConfirmation(ctx, acpSess, e, state.id)
			state.rejected = rejected
			if err != nil {
				return "", err
			}

		case *runtime.ToolCallEvent:
			if _, err := toolCalls.report(ctx, a, acpSess, e.AgentName, e.ToolCall, e.ToolDefinition, acp.ToolCallStatusInProgress); err != nil {
				return "", err
			}

		case *runtime.ToolCallResponseEvent:
			if err := toolCalls.complete(ctx, a, acpSess, e); err != nil {
				return "", err
			}

			if isTodoTool(e.ToolDefinition.Name) && e.Result != nil && e.Result.Meta != nil {
				if planUpdate := buildPlanUpdateFromTodos(e.Result.Meta); planUpdate != nil {
					if err := a.sendUpdate(ctx, acpSess.id, *planUpdate); err != nil {
						return "", err
					}
				}
			}

		case *runtime.ErrorEvent:
			if err := a.sendUpdate(ctx, acpSess.id, acp.UpdateAgentMessageText(fmt.Sprintf("\n\nError: %s\n", e.Error))); err != nil {
				return "", err
			}

		case *runtime.WarningEvent:
			if err := a.sendUpdate(ctx, acpSess.id, acp.UpdateAgentMessageText(fmt.Sprintf("\nWarning: %s\n", e.Message))); err != nil {
				return "", err
			}

		case *runtime.SessionTitleEvent:
			if err := a.sendUpdate(ctx, acpSess.id, acp.SessionUpdate{
				SessionInfoUpdate: &acp.SessionSessionInfoUpdate{
					SessionUpdate: "session_info_update",
					Title:         &e.Title,
				},
			}); err != nil {
				return "", err
			}

		case *runtime.AgentInfoEvent:
			a.refreshCommands(ctx, acpSess)

		case *runtime.TokenUsageEvent:
			if usage != nil {
				if err := a.emitUsage(ctx, acpSess.id, usage); err != nil {
					return "", err
				}
			}

		case *runtime.ModelFallbackEvent:
			if err := a.sendUpdate(ctx, acpSess.id, acp.UpdateAgentMessageText(
				fmt.Sprintf("\nModel %s failed, falling back to %s (%s)\n", e.FailedModel, e.FallbackModel, e.Reason),
			)); err != nil {
				return "", err
			}

		case *runtime.MaxIterationsReachedEvent:
			if err := a.handleMaxIterationsReached(ctx, acpSess, e); err != nil {
				return "", err
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}
	return outcome.result()
}

// handleToolCallConfirmation handles tool call permission requests.
func (a *Agent) handleToolCallConfirmation(ctx context.Context, acpSess *Session, e *runtime.ToolCallConfirmationEvent, id acp.ToolCallId) (bool, error) {
	workingDir, _ := acpSess.workspaceSnapshot()
	toolCallUpdate := buildToolCallUpdate(e.ToolCall, e.ToolDefinition, acp.ToolCallStatusPending, workingDir)
	toolCallUpdate.ToolCallId = id

	permResp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		Meta:      traceMeta(ctx, nil),
		SessionId: acp.SessionId(acpSess.id),
		ToolCall:  toolCallUpdate,
		Options: []acp.PermissionOption{
			{
				Kind:     acp.PermissionOptionKindAllowOnce,
				Name:     "Allow this action",
				OptionId: "allow",
			},
			{
				Kind:     acp.PermissionOptionKindAllowAlways,
				Name:     "Always allow this tool for this session",
				OptionId: "allow-always",
			},
			{
				Kind:     acp.PermissionOptionKindRejectOnce,
				Name:     "Skip this action",
				OptionId: "reject",
			},
		},
	})
	if err != nil {
		return false, err
	}

	if permResp.Outcome.Cancelled != nil {
		acpSess.rt.Resume(ctx, runtime.ResumeRequest{Type: runtime.ResumeTypeReject})
		return true, nil
	}

	if permResp.Outcome.Selected == nil {
		return false, errors.New("unexpected permission outcome")
	}

	switch string(permResp.Outcome.Selected.OptionId) {
	case "allow":
		acpSess.rt.Resume(ctx, runtime.ResumeRequest{Type: runtime.ResumeTypeApprove})
	case "allow-always":
		acpSess.rt.Resume(ctx, runtime.ResumeApproveTool(e.ToolCall.Function.Name))
	case "reject":
		acpSess.rt.Resume(ctx, runtime.ResumeRequest{Type: runtime.ResumeTypeReject})
		return true, nil
	default:
		return false, fmt.Errorf("unexpected permission option: %s", permResp.Outcome.Selected.OptionId)
	}

	return false, nil
}

// handleMaxIterationsReached handles max iterations events.
func (a *Agent) handleMaxIterationsReached(ctx context.Context, acpSess *Session, e *runtime.MaxIterationsReachedEvent) error {
	title := fmt.Sprintf("Maximum iterations (%d) reached", e.MaxIterations)
	permResp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		Meta:      traceMeta(ctx, nil),
		SessionId: acp.SessionId(acpSess.id),
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: "max_iterations",
			Title:      &title,
			Kind:       acp.Ptr(acp.ToolKindExecute),
			Status:     acp.Ptr(acp.ToolCallStatusPending),
		},
		Options: []acp.PermissionOption{
			{
				Kind:     acp.PermissionOptionKindAllowOnce,
				Name:     "Continue",
				OptionId: "continue",
			},
			{
				Kind:     acp.PermissionOptionKindRejectOnce,
				Name:     "Stop",
				OptionId: "stop",
			},
		},
	})
	if err != nil {
		return err
	}

	if permResp.Outcome.Cancelled != nil || permResp.Outcome.Selected == nil ||
		string(permResp.Outcome.Selected.OptionId) != "continue" {
		acpSess.rt.Resume(ctx, runtime.ResumeRequest{Type: runtime.ResumeTypeReject})
	} else {
		acpSess.rt.Resume(ctx, runtime.ResumeRequest{Type: runtime.ResumeTypeApprove})
	}

	return nil
}

func (a *Agent) supportsClientReadTextFile() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.clientFS.ReadTextFile && a.conn != nil
}

func (a *Agent) supportsClientWriteTextFile() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.clientFS.WriteTextFile && a.conn != nil
}

func (a *Agent) resolveSessionPath(sessionID, userPath string) (string, error) {
	a.mu.Lock()
	acpSess := a.sessions[sessionID]
	a.mu.Unlock()
	if acpSess == nil {
		return "", fmt.Errorf("session %s not found", sessionID)
	}

	workingDir, roots := acpSess.pathRoots(a.defaultWorkingDir())
	return resolvePathInRoots(userPath, workingDir, roots)
}

func (a *Agent) defaultWorkingDir() string {
	if a.runConfig != nil && a.runConfig.WorkingDir != "" {
		if wd, err := resolveWorkingDir(a.runConfig.WorkingDir); err == nil {
			return wd
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	wd, err := resolveWorkingDir(cwd)
	if err != nil {
		return cwd
	}
	return wd
}

func (s *Session) workspaceSnapshot() (string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	workingDir := s.workingDir
	if workingDir == "" && s.sess != nil {
		workingDir = s.sess.WorkingDir
	}
	return workingDir, slices.Clone(s.additionalDirs)
}

func (s *Session) pathRoots(fallbackWorkingDir string) (string, []string) {
	workingDir, additionalDirs := s.workspaceSnapshot()
	workingDir = cmp.Or(workingDir, fallbackWorkingDir)

	roots := make([]string, 0, 1+len(additionalDirs))
	if workingDir != "" {
		roots = append(roots, workingDir)
	}
	roots = append(roots, additionalDirs...)
	return workingDir, dedupePaths(roots)
}

func dedupePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		key := normalizePathForComparison(filepath.Clean(path))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, filepath.Clean(path))
	}
	return result
}

// resolveWorkingDir normalizes a working directory path.
func resolveWorkingDir(cwd string) (string, error) {
	wd := strings.TrimSpace(cwd)
	if wd == "" {
		return "", nil
	}
	absWd, err := filepath.Abs(wd)
	if err != nil {
		return "", fmt.Errorf("invalid working directory: %w", err)
	}
	return filepath.Clean(absWd), nil
}

func validateWorkingDir(workingDir string) error {
	if workingDir == "" {
		return nil
	}
	info, err := os.Stat(workingDir)
	if err != nil {
		return fmt.Errorf("working directory does not exist: %w", err)
	}
	if !info.IsDir() {
		return errors.New("working directory must be a directory")
	}
	return nil
}

func resolveAdditionalDirectories(dirs []string) ([]string, error) {
	resolved := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if !filepath.IsAbs(dir) {
			return nil, fmt.Errorf("additional directory must be absolute: %s", dir)
		}
		absDir, err := resolveWorkingDir(dir)
		if err != nil {
			return nil, err
		}
		if err := validateWorkingDir(absDir); err != nil {
			return nil, fmt.Errorf("invalid additional directory %q: %w", dir, err)
		}
		resolved = append(resolved, absDir)
	}
	return dedupePaths(resolved), nil
}
