package runtime

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/skills"
	"github.com/docker/docker-agent/pkg/tools/mcp/oauthflow"
)

// RemoteRuntime implements the Runtime interface using a remote client.
// It works with any client that implements the RemoteClient interface,
// including both HTTP (Client) and Connect-RPC (ConnectRPCClient) clients.
type RemoteRuntime struct {
	client                   RemoteClient
	currentAgent             string
	agentFilename            string
	sessionID                string
	team                     *team.Team
	pendingOAuthElicitations map[string]*ElicitationRequestEvent
	oauthMu                  sync.Mutex

	// pendingModelOverride is the model ref to apply to the current agent
	// on the next [RemoteRuntime.RunStream] call. It is set by
	// [RemoteRuntime.SetAgentModel] and consumed once the override has
	// been forwarded to the server, which persists it server-side as the
	// session's per-agent override.
	pendingMu            sync.Mutex
	pendingModelOverride string

	// resolvedDefault caches the team's default agent name fetched from the
	// server after a successful lookup, so [CurrentAgentName] stays an O(1)
	// field read when no specific agent has been selected.
	resolvedDefault   string
	resolvedDefaultMu sync.Mutex

	stateMu sync.Mutex // sessionID and pendingOAuthElicitations

	reconcileMu       sync.RWMutex // snapshots must not race foreground delivery
	backgroundInit    sync.Mutex
	backgroundMu      sync.Mutex
	backgroundHandler func(Event)
	background        *remoteEventSubscription
	closed            bool
}

// Cursor-based subscriptions are optional so other RemoteClient implementations
// do not accidentally replay foreground history through the background sink.
type remoteEventClient interface {
	GetSessionSnapshot(ctx context.Context, sessionID string) (*api.SessionSnapshotResponse, error)
	StreamSessionEventsSince(ctx context.Context, sessionID string, since uint64) (<-chan Event, error)
}

type remoteEventSubscription struct {
	cancel    context.CancelFunc
	sessionID string
	history   *remoteMessageHistory
}

// Snapshots flatten sub-sessions; recovery exposes only plain assistant text.
type remoteMessageHistory struct {
	mu           sync.Mutex
	content      map[string]*strings.Builder
	sessions     map[string]string
	complete     map[string]bool
	elicitations map[string]bool
	streamDepth  map[string]int
	unidentified bool
	retentionErr error
	bytes        int
}

const (
	remoteHistoryMaxMessages     = 4096
	remoteHistoryMaxBytes        = 8 << 20
	remoteHistoryMaxElicitations = 4096
)

// Eviction cannot turn old answers into new answers: disable recovery on overflow.
func (h *remoteMessageHistory) limit() {
	if h.retentionErr == nil && len(h.content)+len(h.complete) <= remoteHistoryMaxMessages && h.bytes <= remoteHistoryMaxBytes && len(h.elicitations) <= remoteHistoryMaxElicitations && len(h.streamDepth) <= remoteHistoryMaxMessages {
		return
	}
	if h.retentionErr == nil {
		h.retentionErr = errors.New("remote recovery history exceeded its retention limit")
	}
	clear(h.content)
	clear(h.sessions)
	clear(h.streamDepth)
	h.bytes = 0
}

func newRemoteMessageHistory(messages []session.Message) *remoteMessageHistory {
	h := &remoteMessageHistory{
		content:      make(map[string]*strings.Builder),
		sessions:     make(map[string]string),
		complete:     make(map[string]bool),
		elicitations: make(map[string]bool),
		streamDepth:  make(map[string]int),
	}
	for _, msg := range messages {
		if !recoverableAssistantText(msg) {
			continue
		}
		if msg.Message.MessageID == "" {
			h.unidentified = true
		} else {
			h.complete[msg.Message.MessageID] = true
		}
		h.limit()
		if h.retentionErr != nil {
			break
		}
	}
	return h
}

func recoverableAssistantText(msg session.Message) bool {
	return !msg.Implicit && msg.Message.Role == chat.MessageRoleAssistant && chat.VisibleAssistantContent(msg.Message.Content) != "" &&
		len(msg.Message.ToolCalls) == 0 && msg.Message.FunctionCall == nil
}

func (h *remoteMessageHistory) deliver(event Event, send func(Event) bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	request, isElicitation := event.(*ElicitationRequestEvent)
	if isElicitation && request.ElicitationID != "" && h.elicitations[request.ElicitationID] {
		return true
	}
	if isElicitation && request.ElicitationID != "" && len(h.elicitations) >= remoteHistoryMaxElicitations {
		send(Error("remote elicitation history exceeded its retention limit; interaction was not delivered"))
		return false
	}
	choice, ok := event.(*AgentChoiceEvent)
	if ok && choice.MessageID != "" && h.complete[choice.MessageID] {
		return true // already restored from a snapshot
	}
	if !send(event) {
		return false
	}
	if isElicitation && request.ElicitationID != "" {
		h.elicitations[request.ElicitationID] = true
	}
	if started, ok := event.(*StreamStartedEvent); ok && h.retentionErr == nil {
		if depth := h.streamDepth[started.SessionID]; depth < remoteHistoryMaxMessages {
			h.streamDepth[started.SessionID]++
		} else {
			h.retentionErr = errors.New("remote recovery history exceeded its retention limit")
		}
	}
	if stopped, ok := event.(*StreamStoppedEvent); ok {
		if depth := h.streamDepth[stopped.SessionID]; depth > 1 {
			h.streamDepth[stopped.SessionID]--
			return true
		}
		delete(h.streamDepth, stopped.SessionID)
		for id, content := range h.content {
			if scope := h.sessions[id]; scope != "" && scope != stopped.SessionID {
				continue
			}
			if chat.VisibleAssistantContent(content.String()) != "" {
				h.complete[id] = true
			}
			h.bytes -= content.Len()
			delete(h.content, id)
			delete(h.sessions, id)
		}
	}
	if ok && h.retentionErr == nil {
		if choice.MessageID == "" {
			h.unidentified = true
		} else {
			content := h.content[choice.MessageID]
			if content == nil {
				content = new(strings.Builder)
				h.content[choice.MessageID] = content
			}
			content.WriteString(choice.Content)
			h.bytes += len(choice.Content)
			h.sessions[choice.MessageID] = choice.SessionID
		}
	}
	h.limit()
	return true
}

func (h *remoteMessageHistory) reconcile(snapshot *api.SessionSnapshotResponse, send func(Event)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.retentionErr != nil {
		return h.retentionErr
	}
	if h.unidentified {
		return errors.New("cannot safely reconcile assistant messages without message IDs")
	}
	// Validate the whole snapshot before emitting any append-only deltas.
	seen := make(map[string]bool)
	retained := len(h.content) + len(h.complete)
	for _, msg := range snapshot.Messages {
		if !recoverableAssistantText(msg) {
			continue
		}
		id := msg.Message.MessageID
		var delivered string
		if content := h.content[id]; content != nil {
			delivered = chat.VisibleAssistantContent(content.String())
		}
		if id == "" || seen[id] || (!h.complete[id] && !strings.HasPrefix(chat.VisibleAssistantContent(msg.Message.Content), delivered)) {
			return errors.New("cannot safely reconcile changed or ambiguous assistant messages")
		}
		if !h.complete[id] && h.content[id] == nil {
			retained++
		}
		if retained > remoteHistoryMaxMessages || len(seen) >= remoteHistoryMaxMessages {
			return errors.New("remote recovery history exceeded its retention limit")
		}
		seen[id] = true
	}
	for _, msg := range snapshot.Messages {
		if !recoverableAssistantText(msg) {
			continue
		}
		id := msg.Message.MessageID
		if h.complete[id] {
			continue
		}
		var delivered string
		if content := h.content[id]; content != nil {
			delivered = chat.VisibleAssistantContent(content.String())
			h.bytes -= content.Len()
		}
		if suffix := strings.TrimPrefix(chat.VisibleAssistantContent(msg.Message.Content), delivered); suffix != "" {
			sessionID := cmp.Or(h.sessions[id], snapshot.ID)
			send(AgentChoice(msg.AgentName, sessionID, suffix, id))
		}
		delete(h.content, id)
		delete(h.sessions, id)
		h.complete[id] = true
	}
	clear(h.streamDepth)
	h.limit()
	return nil
}

// RemoteRuntimeOption is a function for configuring the RemoteRuntime
type RemoteRuntimeOption func(*RemoteRuntime)

// WithRemoteCurrentAgent sets the current agent name
func WithRemoteCurrentAgent(agentName string) RemoteRuntimeOption {
	return func(r *RemoteRuntime) {
		r.currentAgent = agentName
	}
}

// WithRemoteAgentFilename sets the agent filename to use with the remote API
func WithRemoteAgentFilename(filename string) RemoteRuntimeOption {
	return func(r *RemoteRuntime) {
		r.agentFilename = filename
	}
}

// NewRemoteRuntime creates a new remote runtime that implements the Runtime interface.
// It accepts any client that implements the RemoteClient interface.
func NewRemoteRuntime(client RemoteClient, opts ...RemoteRuntimeOption) (*RemoteRuntime, error) {
	if client == nil {
		return nil, errors.New("client cannot be nil")
	}

	r := &RemoteRuntime{
		client:        client,
		agentFilename: "agent.yaml",
		team:          team.New(),
	}

	for _, opt := range opts {
		opt(r)
	}

	return r, nil
}

// resolvedAgent returns the active agent's name and config from the remote
// team. When no specific agent has been selected, both come from the team's
// first agent — the server owns the notion of "default agent" instead of the
// client hard-coding the historical "root" name.
func (r *RemoteRuntime) resolvedAgent(ctx context.Context) (string, latest.AgentConfig) {
	cfg := r.readCurrentAgentConfig(ctx)
	return cmp.Or(r.currentAgent, cfg.Name), cfg
}

// CurrentAgentName returns the name of the currently active agent.
// When no specific agent has been selected, it falls back to the first agent
// declared by the remote team config. The remote lookup happens at most once;
// the result is cached so subsequent calls are O(1).
func (r *RemoteRuntime) CurrentAgentName(ctx context.Context) string {
	if r.currentAgent != "" {
		return r.currentAgent
	}
	r.resolvedDefaultMu.Lock()
	defer r.resolvedDefaultMu.Unlock()
	if r.resolvedDefault != "" {
		return r.resolvedDefault
	}
	// First successful call performs and caches the remote lookup; detach
	// cancellation so a cancelled caller cannot poison the cached default.
	name, _ := r.resolvedAgent(context.WithoutCancel(ctx))
	if name != "" {
		r.resolvedDefault = name
	}
	return name
}

func (r *RemoteRuntime) CurrentAgentInfo(ctx context.Context) CurrentAgentInfo {
	name, cfg := r.resolvedAgent(ctx)
	return CurrentAgentInfo{
		Name:        name,
		Description: cfg.Description,
		Commands:    cfg.Commands,
	}
}

// SetCurrentAgent sets the currently active agent for subsequent user messages.
// It validates the name against the remote team config; an unknown agent is
// rejected so callers see the same behaviour as LocalRuntime. A failure to
// fetch the team config (network error, auth failure, missing remote) is
// propagated rather than silently accepted — the whole point of this check
// is closing that silent-breakage gap.
func (r *RemoteRuntime) SetCurrentAgent(ctx context.Context, agentName string) error {
	cfg, err := r.client.GetAgent(ctx, r.agentFilename)
	if err != nil {
		return fmt.Errorf("validate agent %q against remote team: %w", agentName, err)
	}
	found := false
	for _, a := range cfg.Agents {
		if a.Name == agentName {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("agent %q not found in remote team", agentName)
	}
	r.currentAgent = agentName
	slog.DebugContext(ctx, "Switched current agent (remote)", "agent", agentName)
	return nil
}

// CurrentAgentTools returns the tools for the current agent from the session.
func (r *RemoteRuntime) CurrentAgentTools(ctx context.Context) ([]tools.Tool, error) {
	sessionID := r.activeSessionID()
	if sessionID == "" {
		return nil, nil
	}
	return r.client.GetSessionTools(ctx, sessionID)
}

// CurrentAgentToolsetStatuses is not implemented for remote runtimes; the
// remote server owns the toolset lifecycle. Returns an empty slice so
// callers (TUI) can show an explanatory empty state without erroring.
func (r *RemoteRuntime) CurrentAgentToolsetStatuses() []tools.ToolsetStatus {
	return nil
}

// RestartToolset restarts a toolset on the remote server.
func (r *RemoteRuntime) RestartToolset(ctx context.Context, toolsetName string) error {
	sessionID := r.activeSessionID()
	if sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.RestartSessionToolset(ctx, sessionID, toolsetName)
}

// EmitStartupInfo emits initial agent, team, and toolset information
func (r *RemoteRuntime) EmitStartupInfo(ctx context.Context, _ *session.Session, events EventSink) {
	agentName, cfg := r.resolvedAgent(ctx)

	events.Emit(AgentInfo(agentName, cfg.Model, cfg.Description, cfg.WelcomeMessage))
	events.Emit(TeamInfo(r.agentDetailsFromConfig(ctx), agentName))

	// Emit a loading indicator while we fetch the real tool count from the server.
	if len(cfg.Toolsets) > 0 {
		events.Emit(ToolsetInfo(0, true, agentName))
	}

	toolCount, err := r.client.GetAgentToolCount(ctx, r.agentFilename, agentName)
	if err != nil {
		slog.WarnContext(ctx, "Failed to get agent tool count", "error", err)
		return
	}

	events.Emit(ToolsetInfo(toolCount, false, agentName))
}

// EmitAgentInfo emits agent and team info without re-fetching tool counts.
func (r *RemoteRuntime) EmitAgentInfo(ctx context.Context, events EventSink) {
	agentName, cfg := r.resolvedAgent(ctx)
	events.Emit(AgentInfo(agentName, cfg.Model, cfg.Description, cfg.WelcomeMessage))
	events.Emit(TeamInfo(r.agentDetailsFromConfig(ctx), agentName))
}

func (r *RemoteRuntime) agentDetailsFromConfig(ctx context.Context) []AgentDetails {
	cfg, err := r.client.GetAgent(ctx, r.agentFilename)
	if err != nil {
		return nil
	}

	var details []AgentDetails
	for _, agent := range cfg.Agents {
		info := AgentDetails{
			Name:        agent.Name,
			Description: agent.Description,
			Commands:    agent.Commands,
		}

		if provider, model, found := strings.Cut(agent.Model, "/"); found {
			info.Provider = provider
			info.Model = model
		} else {
			info.Model = agent.Model
		}

		details = append(details, info)
	}

	return details
}

// readCurrentAgentConfig fetches the active agent's config from the server.
// When no specific agent has been selected, it falls back to the first agent
// in the team — letting the server own the notion of "default agent" instead
// of hard-coding the historical "root" name.
func (r *RemoteRuntime) readCurrentAgentConfig(ctx context.Context) latest.AgentConfig {
	cfg, err := r.client.GetAgent(ctx, r.agentFilename)
	if err != nil || len(cfg.Agents) == 0 {
		return latest.AgentConfig{}
	}

	if r.currentAgent == "" {
		return cfg.Agents[0]
	}

	for _, agent := range cfg.Agents {
		if agent.Name == r.currentAgent {
			return agent
		}
	}

	return latest.AgentConfig{}
}

// RunStream starts the agent's interaction loop and returns a channel of events
func (r *RemoteRuntime) RunStream(ctx context.Context, sess *session.Session) <-chan Event {
	slog.DebugContext(ctx, "Starting remote runtime stream", "agent", r.currentAgent, "session_id", r.activeSessionID())
	events := make(chan Event, defaultEventChannelCapacity)

	go func() {
		defer close(events)
		for !r.reconcileMu.TryRLock() {
			if !waitEventStreamRetry(ctx, 250*time.Millisecond) {
				return
			}
		}
		defer r.reconcileMu.RUnlock()

		messages := r.convertSessionMessages(sess)
		r.stateMu.Lock()
		if r.sessionID != sess.ID {
			clear(r.pendingOAuthElicitations)
		}
		r.sessionID = sess.ID
		r.stateMu.Unlock()
		r.startBackgroundEvents(ctx, sess.ID)

		// Snapshot the queued override but do NOT clear it yet: if the
		// request fails before the server can persist it, clearing here
		// would silently drop the user's switch. We only clear after the
		// server has accepted the request (i.e. RunAgent returned a stream).
		r.pendingMu.Lock()
		model := r.pendingModelOverride
		r.pendingMu.Unlock()

		var streamChan <-chan Event
		var err error

		if r.currentAgent != "" {
			streamChan, err = r.client.RunAgentWithAgentName(ctx, sess.ID, r.agentFilename, r.currentAgent, messages, model)
		} else {
			streamChan, err = r.client.RunAgent(ctx, sess.ID, r.agentFilename, messages, model)
		}

		if err != nil {
			sendClientEvent(ctx, events, Error(fmt.Sprintf("failed to start remote agent: %v", err)))
			return
		}

		// Server accepted the request, so the override (if any) has been
		// forwarded; clear it but only if no concurrent SetAgentModel
		// queued a newer ref while we were dispatching.
		if model != "" {
			r.pendingMu.Lock()
			if r.pendingModelOverride == model {
				r.pendingModelOverride = ""
			}
			r.pendingMu.Unlock()
		}

		// Drain on cancellation too: alternate clients may finish teardown
		// by emitting events after the context is cancelled.
		defer func() {
			for range streamChan {
			}
		}()
		var sawRootStop, sawError bool
		send := func(event Event) bool {
			r.trackOAuthElicitation(event)
			return sendClientEvent(ctx, events, event)
		}
		for streamEvent := range streamChan {
			switch event := streamEvent.(type) {
			case *StreamStoppedEvent:
				sawRootStop = sawRootStop || event.SessionID == "" || event.SessionID == sess.ID
			case *ErrorEvent:
				sawError = true
			}
			r.backgroundMu.Lock()
			subscription := r.background
			r.backgroundMu.Unlock()
			if subscription != nil && subscription.sessionID == sess.ID {
				if !subscription.history.deliver(streamEvent, send) {
					return
				}
			} else if !send(streamEvent) {
				return
			}
		}
		if !sawRootStop && ctx.Err() == nil {
			if !sawError {
				sendClientEvent(ctx, events, Error("remote agent stream ended before completion; the response may be incomplete"))
			}
			sendClientEvent(ctx, events, StreamStopped(sess.ID, r.currentAgent, "error"))
		}
	}()

	return events
}

// Run starts the agent's interaction loop and returns the final messages
func (r *RemoteRuntime) Run(ctx context.Context, sess *session.Session) ([]session.Message, error) {
	ctx, cancel := context.WithCancel(ctx)
	eventsChan := r.RunStream(ctx, sess)
	// Cancel before draining so error returns cannot strand the forwarder.
	defer func() {
		cancel()
		for range eventsChan {
		}
	}()

	for event := range eventsChan {
		if errEvent, ok := event.(*ErrorEvent); ok {
			return nil, fmt.Errorf("%s", errEvent.Error)
		}
	}

	return sess.GetAllMessages(), nil
}

// Steer enqueues a user message for mid-turn injection into the running
// agent loop on the remote server.
func (r *RemoteRuntime) Steer(ctx context.Context, msg QueuedMessage) error {
	sessionID := r.activeSessionID()
	if sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.SteerSession(ctx, sessionID, []api.Message{
		{Content: msg.Content, MultiContent: msg.MultiContent},
	})
}

// FollowUp enqueues a message for end-of-turn processing on the remote server.
func (r *RemoteRuntime) FollowUp(ctx context.Context, msg QueuedMessage) error {
	sessionID := r.activeSessionID()
	if sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.FollowUpSession(ctx, sessionID, []api.Message{
		{Content: msg.Content, MultiContent: msg.MultiContent},
	})
}

func (r *RemoteRuntime) QueueStatus() QueueStatus {
	return QueueStatus{}
}

// Resume allows resuming execution after user confirmation
func (r *RemoteRuntime) Resume(ctx context.Context, req ResumeRequest) {
	sessionID := r.activeSessionID()
	slog.DebugContext(ctx, "Resuming remote runtime", "agent", r.currentAgent, "type", req.Type, "reason", req.Reason, "tool_name", req.ToolName, "session_id", sessionID)

	if sessionID == "" {
		slog.ErrorContext(ctx, "Cannot resume: no session ID available")
		return
	}

	if err := r.client.ResumeSession(ctx, sessionID, string(req.Type), req.Reason, req.ToolName); err != nil {
		slog.ErrorContext(ctx, "Failed to resume remote session", "error", err, "session_id", sessionID)
	}
}

// Summarize generates a summary for the session by compacting it server-side.
func (r *RemoteRuntime) Summarize(ctx context.Context, sess *session.Session, _ string, sink EventSink) {
	sessionID := r.activeSessionID()
	if sessionID == "" {
		sink.Emit(SessionSummary(sess.ID, "No active session to summarize", r.currentAgent, 0, 0, "", nil))
		return
	}
	if err := r.client.CompactSession(ctx, sessionID); err != nil {
		slog.WarnContext(ctx, "Failed to compact session", "error", err)
		sink.Emit(SessionSummary(sess.ID, fmt.Sprintf("Compaction failed: %v", err), r.currentAgent, 0, 0, "", nil))
		return
	}
	sink.Emit(SessionSummary(sess.ID, "Session compacted successfully", r.currentAgent, 0, 0, "", nil))
}

func (r *RemoteRuntime) convertSessionMessages(sess *session.Session) []api.Message {
	sessionMessages := sess.GetAllMessages()
	messages := make([]api.Message, 0, len(sessionMessages))

	for i := range sessionMessages {
		if sessionMessages[i].Message.Role == chat.MessageRoleUser || sessionMessages[i].Message.Role == chat.MessageRoleAssistant {
			messages = append(messages, api.Message{
				Role:    sessionMessages[i].Message.Role,
				Content: sessionMessages[i].Message.Content,
			})
		}
	}

	return messages
}

func (r *RemoteRuntime) trackOAuthElicitation(event Event) {
	request, ok := event.(*ElicitationRequestEvent)
	if !ok || request.Meta["docker-agent/type"] != "oauth_flow" || request.ElicitationID == "" {
		return
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.pendingOAuthElicitations == nil {
		r.pendingOAuthElicitations = make(map[string]*ElicitationRequestEvent)
	}
	if r.pendingOAuthElicitations[request.ElicitationID] == nil && len(r.pendingOAuthElicitations) < remoteHistoryMaxElicitations {
		r.pendingOAuthElicitations[request.ElicitationID] = request
	}
}

// ResumeElicitation answers a request, running OAuth only for its explicit ID.
func (r *RemoteRuntime) ResumeElicitation(ctx context.Context, action tools.ElicitationAction, content map[string]any, elicitationID ...string) error {
	// Serialize responses, including the browser flow, so concurrent accepts
	// cannot authorize the same request twice.
	r.oauthMu.Lock()
	defer r.oauthMu.Unlock()
	id := firstElicitationID(elicitationID)
	r.stateMu.Lock()
	sessionID := r.sessionID
	pending := r.pendingOAuthElicitations[id]
	ambiguous := id == "" && len(r.pendingOAuthElicitations) > 0
	r.stateMu.Unlock()
	if ambiguous {
		return errors.New("OAuth elicitation requires an explicit elicitation ID")
	}
	if pending != nil {
		defer func() {
			r.stateMu.Lock()
			defer r.stateMu.Unlock()
			delete(r.pendingOAuthElicitations, id)
		}()
		if action == tools.ElicitationActionAccept {
			// handleOAuthElicitation sends the token response itself.
			return r.handleOAuthElicitation(ctx, sessionID, pending)
		}
	}
	return r.client.ResumeElicitation(ctx, sessionID, action, content, id)
}

func (r *RemoteRuntime) handleOAuthElicitation(ctx context.Context, sessionID string, req *ElicitationRequestEvent) error {
	if req == nil {
		return nil
	}

	slog.DebugContext(ctx, "Handling OAuth elicitation request", "server_url", req.Meta["docker-agent/server_url"])

	serverURL, ok := req.Meta["docker-agent/server_url"].(string)
	if !ok {
		err := errors.New("server_url missing from elicitation metadata")
		slog.ErrorContext(ctx, "Failed to extract server_url", "error", err)
		_ = r.client.ResumeElicitation(ctx, sessionID, "decline", nil, req.ElicitationID)
		return err
	}

	authServerMetadata, ok := req.Meta["auth_server_metadata"].(map[string]any)
	if !ok {
		err := errors.New("auth_server_metadata missing from elicitation metadata")
		slog.ErrorContext(ctx, "Failed to extract auth_server_metadata", "error", err)
		_ = r.client.ResumeElicitation(ctx, sessionID, "decline", nil, req.ElicitationID)
		return err
	}

	var authMetadata oauthflow.AuthorizationServerMetadata
	metadataBytes, err := json.Marshal(authServerMetadata)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to marshal auth_server_metadata", "error", err)
		_ = r.client.ResumeElicitation(ctx, sessionID, "decline", nil, req.ElicitationID)
		return fmt.Errorf("failed to marshal auth_server_metadata: %w", err)
	}
	if err := json.Unmarshal(metadataBytes, &authMetadata); err != nil {
		slog.ErrorContext(ctx, "Failed to unmarshal auth_server_metadata", "error", err)
		_ = r.client.ResumeElicitation(ctx, sessionID, "decline", nil, req.ElicitationID)
		return fmt.Errorf("failed to unmarshal auth_server_metadata: %w", err)
	}

	resourceIndicator := serverURL
	if resourceMetadata, ok := req.Meta["resource_metadata"].(map[string]any); ok {
		if resource, ok := resourceMetadata["resource"].(string); ok && resource != "" {
			resourceIndicator = resource
		}
	}

	slog.DebugContext(ctx, "Authorization server metadata extracted", "issuer", authMetadata.Issuer)

	oauthCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	slog.DebugContext(ctx, "Creating OAuth callback server")
	callbackServer := oauthflow.NewCallbackServer()
	defer func() {
		// Detach from ctx's cancellation (the request may be done) but
		// keep its trace context for the shutdown.
		shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer shutdownCancel()
		if err := callbackServer.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(ctx, "Failed to shutdown callback server", "error", err)
		}
	}()

	if err := callbackServer.Start(ctx); err != nil {
		slog.ErrorContext(ctx, "Failed to start callback server", "error", err)
		_ = r.client.ResumeElicitation(ctx, sessionID, "decline", nil, req.ElicitationID)
		return fmt.Errorf("failed to start callback server: %w", err)
	}

	redirectURI := callbackServer.GetRedirectURI()
	slog.DebugContext(ctx, "Callback server started", "redirect_uri", redirectURI)

	var clientID, clientSecret, authMethod string
	if authMetadata.RegistrationEndpoint != "" {
		slog.DebugContext(ctx, "Attempting dynamic client registration")
		clientID, clientSecret, authMethod, err = oauthflow.RegisterClient(oauthCtx, &authMetadata, redirectURI, nil)
		if err != nil {
			slog.ErrorContext(ctx, "Dynamic client registration failed", "error", err)
			_ = r.client.ResumeElicitation(ctx, sessionID, "decline", nil, req.ElicitationID)
			return fmt.Errorf("failed to register client: %w", err)
		}
		slog.DebugContext(ctx, "Client registered successfully", "client_id", clientID)
	} else {
		err := errors.New("authorization server does not support dynamic client registration")
		slog.ErrorContext(ctx, "Client registration not supported", "error", err)
		_ = r.client.ResumeElicitation(ctx, sessionID, "decline", nil, req.ElicitationID)
		return err
	}

	state, err := oauthflow.GenerateState()
	if err != nil {
		slog.ErrorContext(ctx, "Failed to generate state", "error", err)
		_ = r.client.ResumeElicitation(ctx, sessionID, "decline", nil, req.ElicitationID)
		return fmt.Errorf("failed to generate state: %w", err)
	}

	callbackServer.SetExpectedState(state)
	verifier := oauthflow.GeneratePKCEVerifier()

	authURL := oauthflow.BuildAuthorizationURL(
		authMetadata.AuthorizationEndpoint,
		clientID,
		redirectURI,
		state,
		oauth2.S256ChallengeFromVerifier(verifier),
		resourceIndicator,
		nil,
	)

	slog.DebugContext(ctx, "Authorization URL built", "url", authURL)

	slog.DebugContext(ctx, "Requesting authorization code")
	code, receivedState, err := oauthflow.RequestAuthorizationCode(oauthCtx, authURL, callbackServer, state)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to get authorization code", "error", err)
		_ = r.client.ResumeElicitation(ctx, sessionID, "decline", nil, req.ElicitationID)
		return fmt.Errorf("failed to get authorization code: %w", err)
	}

	if receivedState != state {
		err := fmt.Errorf("state mismatch: expected %s, got %s", state, receivedState)
		slog.ErrorContext(ctx, "State mismatch in authorization response", "error", err)
		_ = r.client.ResumeElicitation(ctx, sessionID, "decline", nil, req.ElicitationID)
		return err
	}

	slog.DebugContext(ctx, "Authorization code received, exchanging for token")

	token, err := oauthflow.ExchangeCodeForTokenWithClient(
		oauthCtx,
		oauthflow.DefaultHTTPClient(),
		authMetadata.TokenEndpoint,
		code,
		verifier,
		clientID,
		clientSecret,
		redirectURI,
		resourceIndicator,
		authMethod,
	)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to exchange code for token", "error", err)
		_ = r.client.ResumeElicitation(ctx, sessionID, "decline", nil, req.ElicitationID)
		return fmt.Errorf("failed to exchange code for token: %w", err)
	}

	slog.DebugContext(ctx, "Token obtained successfully", "token_type", token.TokenType)

	tokenData := map[string]any{
		"access_token": token.AccessToken,
		"token_type":   token.TokenType,
	}
	if token.ExpiresIn > 0 {
		tokenData["expires_in"] = token.ExpiresIn
	}
	if token.RefreshToken != "" {
		tokenData["refresh_token"] = token.RefreshToken
	}

	slog.DebugContext(ctx, "Sending token to server")
	if err := r.client.ResumeElicitation(ctx, sessionID, tools.ElicitationActionAccept, tokenData, req.ElicitationID); err != nil {
		slog.ErrorContext(ctx, "Failed to send token to server", "error", err)
		return fmt.Errorf("failed to send token to server: %w", err)
	}

	slog.DebugContext(ctx, "OAuth flow completed successfully")
	return nil
}

// SessionStore returns a RemoteSessionStore that wraps the remote client.
func (r *RemoteRuntime) SessionStore() session.Store {
	return NewRemoteSessionStore(r.client)
}

// AvailableModels returns available models for the agent.
func (r *RemoteRuntime) AvailableModels(ctx context.Context) []ModelChoice {
	models, err := r.client.GetAvailableModels(ctx)
	if err != nil {
		slog.WarnContext(ctx, "Failed to get available models", "error", err)
		return nil
	}
	choices := make([]ModelChoice, len(models))
	for i, m := range models {
		choices[i] = ModelChoice{Name: m, Ref: m}
	}
	return choices
}

// SetAgentModel queues modelRef as the override to apply on the session's
// current agent. The override is forwarded to the server on the next
// [RemoteRuntime.RunStream] call, where the server persists it as the
// per-agent model override (same effect as the historic dedicated endpoint,
// just without the extra round trip). A subsequent call before the next
// turn replaces the queued ref; an empty string clears it.
func (r *RemoteRuntime) SetAgentModel(_ context.Context, _, modelRef string) error {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	r.pendingModelOverride = modelRef
	return nil
}

// CycleAgentThinkingLevel is unsupported on remote runtimes; the server owns
// model configuration including thinking-effort selection.
func (r *RemoteRuntime) CycleAgentThinkingLevel(context.Context, string) (effort.Level, error) {
	return "", ErrUnsupported
}

// SetAgentThinkingLevel is unsupported on remote runtimes; the server owns
// model configuration including thinking-effort selection.
func (r *RemoteRuntime) SetAgentThinkingLevel(context.Context, string, effort.Level) (effort.Level, error) {
	return "", ErrUnsupported
}

// SupportsModelSwitching returns true for remote runtimes (model switching is handled server-side).
func (r *RemoteRuntime) SupportsModelSwitching() bool {
	return true
}

// PermissionsInfo returns nil for remote runtime since permissions are handled server-side.
func (r *RemoteRuntime) PermissionsInfo() *PermissionsInfo {
	return nil
}

// ResetStartupInfo is a no-op for remote runtime.
func (r *RemoteRuntime) ResetStartupInfo() {
}

// CurrentAgentSkillsToolset returns nil for remote runtimes since skills are managed server-side.
func (r *RemoteRuntime) CurrentAgentSkillsToolset() *skills.ToolSet {
	return nil
}

func (r *RemoteRuntime) ReadSkillContent(context.Context, *session.Session, string) (string, error) {
	return "", ErrUnsupported
}

// RunSkillFork is unsupported on remote runtimes; the server owns skill
// execution.
func (r *RemoteRuntime) RunSkillFork(context.Context, *session.Session, skills.RunSkillArgs, EventSink) (*tools.ToolCallResult, error) {
	return nil, fmt.Errorf("run skill fork: %w", ErrUnsupported)
}

// UpdateSessionTitle updates the title of the current session on the remote server.
func (r *RemoteRuntime) UpdateSessionTitle(ctx context.Context, sess *session.Session, title string) error {
	sessionID := r.activeSessionID()
	sess.SetTitle(title)
	if sessionID == "" {
		return errors.New("cannot update session title: no session ID available")
	}
	return r.client.UpdateSessionTitle(ctx, sessionID, title)
}

// CurrentMCPPrompts returns available MCP prompts from the server.
func (r *RemoteRuntime) CurrentMCPPrompts(ctx context.Context) map[string]tools.PromptInfo {
	sessionID := r.activeSessionID()
	if sessionID == "" {
		return make(map[string]tools.PromptInfo)
	}
	prompts, err := r.client.GetSessionMCPPrompts(ctx, sessionID)
	if err != nil {
		slog.WarnContext(ctx, "Failed to get MCP prompts", "error", err)
		return make(map[string]tools.PromptInfo)
	}
	// The client decodes the JSON response into map[string]any, so each
	// value is a map[string]any, never a concrete PromptInfo; round-trip
	// through JSON to get typed values.
	result := make(map[string]tools.PromptInfo)
	for k, v := range prompts {
		b, err := json.Marshal(v)
		if err != nil {
			slog.WarnContext(ctx, "Failed to convert MCP prompt info", "prompt", k, "error", err)
			continue
		}
		var info tools.PromptInfo
		if err := json.Unmarshal(b, &info); err != nil {
			slog.WarnContext(ctx, "Failed to convert MCP prompt info", "prompt", k, "error", err)
			continue
		}
		result[k] = info
	}
	return result
}

// ExecuteMCPPrompt executes an MCP prompt on the server.
func (r *RemoteRuntime) ExecuteMCPPrompt(ctx context.Context, promptName string, args map[string]string) (string, error) {
	sessionID := r.activeSessionID()
	if sessionID == "" {
		return "", errors.New("no active session")
	}
	return r.client.ExecuteSessionMCPPrompt(ctx, sessionID, promptName, args)
}

// TitleGenerator is not supported on remote runtimes (titles are generated server-side).
func (r *RemoteRuntime) TitleGenerator(context.Context) *sessiontitle.Generator {
	return nil
}

// TogglePause pauses/resumes a session on the server.
func (r *RemoteRuntime) TogglePause(ctx context.Context) (bool, error) {
	sessionID := r.activeSessionID()
	if sessionID == "" {
		return false, errors.New("no active session")
	}
	return false, r.client.PauseSession(ctx, sessionID)
}

// OnToolsChanged is a no-op for remote runtimes; tool-list changes are
// observed server-side and surface through the run-stream events rather
// than via an out-of-band callback.
func (r *RemoteRuntime) OnToolsChanged(func(Event)) {}

// OnBackgroundEvent receives server-side recalls and out-of-band events.
// The subscription outlives individual turns and stops on Close or unregister.
func (r *RemoteRuntime) OnBackgroundEvent(handler func(Event)) {
	r.backgroundMu.Lock()
	defer r.backgroundMu.Unlock()
	r.backgroundHandler = handler
	if handler == nil && r.background != nil {
		r.background.cancel()
		r.background = nil
	}
}

func (r *RemoteRuntime) activeSessionID() string {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.sessionID
}

func (r *RemoteRuntime) deliverBackgroundEvent(subscription *remoteEventSubscription, event Event) bool {
	r.backgroundMu.Lock()
	handler := r.backgroundHandler
	active := r.background == subscription && !r.closed
	r.backgroundMu.Unlock()
	if !active || handler == nil {
		return false
	}
	if recovered, ok := event.(*SessionRecoveredEvent); ok {
		r.stateMu.Lock()
		// Root idleness does not complete detached sub-session requests.
		for id, request := range r.pendingOAuthElicitations {
			if request.SessionID == "" || request.SessionID == recovered.SessionID {
				delete(r.pendingOAuthElicitations, id)
			}
		}
		r.stateMu.Unlock()
	} else {
		r.trackOAuthElicitation(event)
	}
	handler(event)
	return true
}

func (r *RemoteRuntime) emitBackgroundEvent(subscription *remoteEventSubscription, event Event) bool {
	return subscription.history.deliver(event, func(event Event) bool {
		return r.deliverBackgroundEvent(subscription, event)
	})
}

func (r *RemoteRuntime) clearBackgroundSubscription(subscription *remoteEventSubscription) {
	subscription.cancel()
	r.backgroundMu.Lock()
	defer r.backgroundMu.Unlock()
	if r.background == subscription {
		r.background = nil
	}
}

func (r *RemoteRuntime) startBackgroundEvents(ctx context.Context, sessionID string) {
	client, ok := r.client.(remoteEventClient)
	if !ok {
		return
	}
	r.backgroundInit.Lock()
	defer r.backgroundInit.Unlock()
	r.backgroundMu.Lock()
	if r.closed || r.backgroundHandler == nil {
		r.backgroundMu.Unlock()
		return
	}
	if r.background != nil && r.background.sessionID == sessionID {
		r.backgroundMu.Unlock()
		return
	}
	if r.background != nil {
		r.background.cancel()
	}
	backgroundCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	subscription := &remoteEventSubscription{cancel: cancel, sessionID: sessionID, history: newRemoteMessageHistory(nil)}
	r.background = subscription
	r.backgroundMu.Unlock()

	// Capture the cursor BEFORE submitting a turn; no old answers are replayed.
	snapshotCtx, cancelSnapshot := context.WithCancel(ctx)
	stop := context.AfterFunc(backgroundCtx, cancelSnapshot)
	snapshot, err := client.GetSessionSnapshot(snapshotCtx, sessionID)
	stop()
	cancelSnapshot()
	if err == nil && (snapshot == nil || snapshot.ID != sessionID) {
		err = errors.New("snapshot session ID does not match subscription")
	}
	if err == nil && snapshot.Streaming {
		err = errors.New("session is already streaming; no safe background baseline")
	}
	if err != nil {
		r.emitBackgroundEvent(subscription, Warning(fmt.Sprintf("remote background events unavailable: %v", err), ""))
		r.clearBackgroundSubscription(subscription)
		return
	}
	r.backgroundMu.Lock()
	if r.background != subscription || r.closed {
		r.backgroundMu.Unlock()
		cancel()
		return
	}
	subscription.history = newRemoteMessageHistory(snapshot.Messages)
	r.backgroundMu.Unlock()

	go func() {
		defer r.clearBackgroundSubscription(subscription)
		events, err := r.openBackgroundEvents(backgroundCtx, client, sessionID, snapshot.LastEventSeq)
		if err != nil {
			r.emitBackgroundEvent(subscription, Error(fmt.Sprintf("subscribing to remote background events: %v", err)))
			return
		}
		for {
			select {
			case <-backgroundCtx.Done():
				return
			case event, ok := <-events:
				if !ok {
					return // session ended; never blindly replay old history
				}
				if failure, ok := event.(*ErrorEvent); ok && failure.Error == sessionEventGapError {
					r.emitBackgroundEvent(subscription, Warning("remote event gap; recovering saved assistant text when idle (tool and interaction events may be incomplete)", ""))
					if !waitEventStreamRetry(backgroundCtx, 250*time.Millisecond) {
						return
					}
					var err error
					snapshot, err = r.reconcileBackgroundSnapshot(backgroundCtx, subscription, client)
					if err != nil {
						r.emitBackgroundEvent(subscription, Error(fmt.Sprintf("recovering remote event gap: %v", err)))
						return
					}
					events, err = r.openBackgroundEvents(backgroundCtx, client, sessionID, snapshot.LastEventSeq)
					if err != nil {
						r.emitBackgroundEvent(subscription, Error(fmt.Sprintf("resuming remote background events: %v", err)))
						return
					}
					continue
				}
				if !r.emitBackgroundEvent(subscription, event) {
					return
				}
			}
		}
	}()
}

// /events may not exist until the first recall or elicitation creates its log.
func (r *RemoteRuntime) openBackgroundEvents(ctx context.Context, client remoteEventClient, sessionID string, since uint64) (<-chan Event, error) {
	delay := 250 * time.Millisecond
	for {
		events, err := client.StreamSessionEventsSince(ctx, sessionID, since)
		if err == nil {
			return events, nil
		}
		var httpErr *sessionEventHTTPError
		if errors.As(err, &httpErr) && httpErr.status >= 400 && httpErr.status < 500 && httpErr.status != http.StatusNotFound && httpErr.status != http.StatusTooManyRequests {
			return nil, err
		}
		if !waitEventStreamRetry(ctx, delay) {
			return nil, ctx.Err()
		}
		delay = min(2*delay, 5*time.Second)
	}
}

func (r *RemoteRuntime) reconcileBackgroundSnapshot(ctx context.Context, subscription *remoteEventSubscription, client remoteEventClient) (*api.SessionSnapshotResponse, error) {
	for {
		if !r.reconcileMu.TryLock() {
			if !waitEventStreamRetry(ctx, 250*time.Millisecond) {
				return nil, ctx.Err()
			}
			continue
		}
		snapshot, err := r.reconcileIdleSnapshot(ctx, subscription, client)
		r.reconcileMu.Unlock()
		if err != nil || !snapshot.Streaming {
			return snapshot, err
		}
		if !waitEventStreamRetry(ctx, 250*time.Millisecond) {
			return nil, ctx.Err()
		}
	}
}

func (r *RemoteRuntime) reconcileIdleSnapshot(ctx context.Context, subscription *remoteEventSubscription, client remoteEventClient) (*api.SessionSnapshotResponse, error) {
	snapshot, err := client.GetSessionSnapshot(ctx, subscription.sessionID)
	if err != nil {
		return nil, err
	}
	if snapshot == nil || snapshot.ID != subscription.sessionID {
		return nil, errors.New("snapshot session ID does not match subscription")
	}
	if snapshot.Streaming {
		return snapshot, nil // partial saved messages cannot define a safe cursor
	}
	r.backgroundMu.Lock()
	handler := r.backgroundHandler
	active := r.background == subscription && !r.closed
	r.backgroundMu.Unlock()
	if !active || handler == nil || ctx.Err() != nil {
		return nil, context.Canceled
	}
	if err := subscription.history.reconcile(snapshot, func(event Event) {
		r.deliverBackgroundEvent(subscription, event)
	}); err != nil {
		return nil, err
	}
	if !r.deliverBackgroundEvent(subscription, SessionRecovered(snapshot.ID)) {
		return nil, context.Canceled
	}
	return snapshot, nil
}

// OnElicitationRequest is a no-op for remote runtimes; elicitation requests
// arrive on RunStream, or through OnBackgroundEvent when a cursor-based
// subscription is active, so there is no additional sink to register.
//
// RemoteRuntime deliberately does NOT implement
// LocalRuntime.MirrorsElicitationOnRunStream: embedders that forward
// RunStream events verbatim (e.g. pkg/app.App) rely on that capability check
// to forward RunStream elicitations when no background subscription is active.
func (r *RemoteRuntime) OnElicitationRequest(func(Event)) {}

// RetireBackgroundEvents stops deliveries for a replaced conversation.
func (r *RemoteRuntime) RetireBackgroundEvents() {
	r.backgroundMu.Lock()
	defer r.backgroundMu.Unlock()
	if r.background != nil {
		r.background.cancel()
		r.background = nil
	}
}

// Close stops the out-of-band session subscription.
func (r *RemoteRuntime) Close() error {
	r.backgroundMu.Lock()
	defer r.backgroundMu.Unlock()
	r.closed = true
	r.backgroundHandler = nil
	if r.background != nil {
		r.background.cancel()
		r.background = nil
	}
	return nil
}

// GetSnapshots retrieves available snapshots for the current session.
func (r *RemoteRuntime) GetSnapshots(ctx context.Context) ([]map[string]any, error) {
	sessionID := r.activeSessionID()
	if sessionID == "" {
		return nil, errors.New("no active session")
	}
	return r.client.GetSessionSnapshots(ctx, sessionID)
}

// Undo reverts to the previous snapshot on the remote server.
func (r *RemoteRuntime) Undo(ctx context.Context) error {
	sessionID := r.activeSessionID()
	if sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.UndoSession(ctx, sessionID)
}

// Reset resets the session to its initial state on the remote server.
func (r *RemoteRuntime) Reset(ctx context.Context) error {
	sessionID := r.activeSessionID()
	if sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.ResetSession(ctx, sessionID)
}

// AddMessageToSession adds a message to the current session on the remote server.
func (r *RemoteRuntime) AddMessageToSession(ctx context.Context, msg *session.Message) error {
	sessionID := r.activeSessionID()
	if sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.AddMessage(ctx, sessionID, msg)
}

// UpdateSessionMessage updates a message in the current session on the remote server.
func (r *RemoteRuntime) UpdateSessionMessage(ctx context.Context, msgID string, msg *session.Message) error {
	sessionID := r.activeSessionID()
	if sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.UpdateMessage(ctx, sessionID, msgID, msg)
}

// AddSessionSummary adds a summary item to the current session on the remote server.
func (r *RemoteRuntime) AddSessionSummary(ctx context.Context, item session.Item) error {
	sessionID := r.activeSessionID()
	if sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.AddSummary(ctx, sessionID, item)
}

// UpdateSessionTokens updates token counts for the current session on the remote server.
func (r *RemoteRuntime) UpdateSessionTokens(ctx context.Context, inputTokens, outputTokens int64, cost float64) error {
	sessionID := r.activeSessionID()
	if sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.UpdateSessionTokens(ctx, sessionID, inputTokens, outputTokens, cost)
}

// SetSessionStarred sets the starred status for the current session on the remote server.
func (r *RemoteRuntime) SetSessionStarred(ctx context.Context, starred bool) error {
	sessionID := r.activeSessionID()
	if sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.SetSessionStarred(ctx, sessionID, starred)
}

var _ Runtime = (*RemoteRuntime)(nil)

// RemoteSessionStore wraps a RemoteClient to implement the session.Store interface.
type RemoteSessionStore struct {
	client RemoteClient
}

// NewRemoteSessionStore creates a new RemoteSessionStore.
func NewRemoteSessionStore(client RemoteClient) *RemoteSessionStore {
	return &RemoteSessionStore{client: client}
}

func (s *RemoteSessionStore) AddSession(context.Context, *session.Session) error {
	return fmt.Errorf("add session: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) GetSession(context.Context, string) (*session.Session, error) {
	return nil, fmt.Errorf("get session: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) GetSessionByOrigin(context.Context, string, string) (*session.Session, error) {
	return nil, fmt.Errorf("get session by origin: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) GetSessions(ctx context.Context) ([]*session.Session, error) {
	sessions, err := s.client.GetAllSessions(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]*session.Session, len(sessions))
	for i := range sessions {
		result[i] = &sessions[i]
	}
	return result, nil
}

func (s *RemoteSessionStore) GetSessionSummaries(context.Context) ([]session.Summary, error) {
	return nil, fmt.Errorf("get session summaries: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) DeleteSession(ctx context.Context, id string) error {
	return s.client.DeleteRemoteSession(ctx, id)
}

func (s *RemoteSessionStore) UpdateSession(context.Context, *session.Session) error {
	return fmt.Errorf("update session: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) SetSessionStarred(context.Context, string, bool) error {
	return fmt.Errorf("set session starred: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) AddMessage(context.Context, string, *session.Message) (int64, error) {
	return 0, fmt.Errorf("add message: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) UpdateMessage(context.Context, string, int64, *session.Message) error {
	return fmt.Errorf("update message: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) AddSubSession(context.Context, string, *session.Session) error {
	return fmt.Errorf("add sub session: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) PersistCompaction(context.Context, *session.Session, int64, int64, session.Item) error {
	return fmt.Errorf("persist compaction: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) AddSummary(context.Context, string, session.Item) error {
	return fmt.Errorf("add summary: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) AddError(context.Context, string, *session.Error) error {
	return fmt.Errorf("add error: %w", ErrUnsupported)
}

// AddEvaluation is a no-op: the server persists runtime evaluation events.
func (s *RemoteSessionStore) AddEvaluation(context.Context, string, *session.Evaluation) error {
	return nil
}

// AddRoutingDecision is a no-op: the server persists runtime routing events.
func (s *RemoteSessionStore) AddRoutingDecision(context.Context, string, *session.RoutingDecision) error {
	return nil
}

func (s *RemoteSessionStore) UpdateSessionTokens(context.Context, string, int64, int64, float64) error {
	return fmt.Errorf("update session tokens: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) UpdateSessionTitle(context.Context, string, string) error {
	return fmt.Errorf("update session title: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) Close() error {
	return nil
}

var _ session.Store = (*RemoteSessionStore)(nil)
