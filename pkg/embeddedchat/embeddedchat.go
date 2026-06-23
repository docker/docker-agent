// Package embeddedchat provides a small headless chat wrapper around the
// docker-agent runtime for embedders that want to drive an agent from their
// own UI instead of running docker-agent's Bubble Tea application.
package embeddedchat

import (
	"context"
	"errors"
	"fmt"
	"sync"

	dagentcfg "github.com/docker/docker-agent/pkg/config"
	dagentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
	"github.com/docker/docker-agent/pkg/tools"
)

const defaultEventBuffer = 32

var (
	// ErrAgentSourceRequired is returned when New is called without an agent source.
	ErrAgentSourceRequired = errors.New("embeddedchat: agent source is required")
	// ErrNotInitialized is returned when a Session has no runtime or conversation.
	ErrNotInitialized = errors.New("embeddedchat: session is not initialized")
	// ErrRunActive is returned when Send is called while a previous run is still active.
	ErrRunActive = errors.New("embeddedchat: a run is already active")
	// ErrClosed is returned when an operation is attempted after Close.
	ErrClosed = errors.New("embeddedchat: session is closed")
)

// Config describes an embedded agent session.
type Config struct {
	// AgentSource is the agent/team definition to load. Bytes sources are a
	// good fit for embedders that ship a pinned agent in their binary.
	AgentSource dagentcfg.Source
	// RuntimeConfig is passed to the team loader. When nil, a zero runtime
	// config is used.
	RuntimeConfig *dagentcfg.RuntimeConfig
	// ToolsetRegistry resolves toolsets declared by AgentSource. When nil,
	// docker-agent's full YAML-loading registry is used.
	ToolsetRegistry teamloader.ToolsetRegistry
	// RuntimeOptions are appended when constructing the runtime.
	RuntimeOptions []dagentruntime.Opt
	// SessionOptions are appended when constructing each conversation session.
	SessionOptions []session.Opt
	// EventBuffer controls the size of the channel returned by Send. When zero,
	// a small default buffer is used.
	EventBuffer int
}

// Event is the UI-friendly form of one runtime stream event.
type Event struct {
	// Text is an assistant text delta.
	Text string
	// Tool describes a tool call starting, awaiting confirmation, or finishing.
	Tool *ToolActivity
	// Err is a user-facing runtime error.
	Err error
	// Done marks a clean end of the reply stream.
	Done bool
	// RuntimeEvent is the original docker-agent runtime event for projected
	// events. Not every runtime event is forwarded by this compact API; callers
	// that need the full raw stream can use Runtime().RunStream directly.
	RuntimeEvent dagentruntime.Event
}

// ToolActivity describes one tool call surfaced by the runtime.
type ToolActivity struct {
	Call     tools.ToolCall
	Def      tools.Tool
	Finished bool
	IsError  bool
	// NeedsConfirmation is true when the runtime is blocked until Confirm is
	// called with the user's decision.
	NeedsConfirmation bool
}

// runtimeRunner is the subset of runtime.Runtime the headless session needs.
type runtimeRunner interface {
	RunStream(ctx context.Context, sess *session.Session) <-chan dagentruntime.Event
	Resume(ctx context.Context, req dagentruntime.ResumeRequest)
	ResumeElicitation(ctx context.Context, action tools.ElicitationAction, content map[string]any) error
	Close() error
}

// Session owns one embedded runtime and one mutable conversation session.
type Session struct {
	cfg Config

	rt      runtimeRunner
	session *session.Session
	welcome string

	mu           sync.Mutex
	activeCancel context.CancelFunc
	activeRun    int
	closed       bool
}

// New loads the configured agent and creates a fresh conversation session.
func New(ctx context.Context, cfg Config) (*Session, error) {
	if cfg.AgentSource == nil {
		return nil, ErrAgentSourceRequired
	}
	runConfig := cfg.RuntimeConfig
	if runConfig == nil {
		runConfig = &dagentcfg.RuntimeConfig{}
	}

	loadOpts := loaderdefaults.Opts()
	if cfg.ToolsetRegistry != nil {
		loadOpts = append(loadOpts, teamloader.WithToolsetRegistry(cfg.ToolsetRegistry))
	}
	loaded, err := teamloader.LoadWithConfig(ctx, cfg.AgentSource, runConfig, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("embeddedchat: load agent: %w", err)
	}

	modelSwitcher := &dagentruntime.ModelSwitcherConfig{
		Models:             loaded.Models,
		Providers:          loaded.Providers,
		ModelsGateway:      runConfig.ModelsGateway,
		EnvProvider:        runConfig.EnvProvider(),
		ProviderRegistry:   loaded.ProviderRegistry,
		AgentDefaultModels: loaded.AgentDefaultModels,
	}

	runtimeOpts := []dagentruntime.Opt{
		dagentruntime.WithModelSwitcherConfig(modelSwitcher),
		dagentruntime.WithWorkingDir(runConfig.WorkingDir),
		dagentruntime.WithSessionStore(session.NewInMemorySessionStore()),
	}
	runtimeOpts = append(runtimeOpts, cfg.RuntimeOptions...)
	rt, err := dagentruntime.New(loaded.Team, runtimeOpts...)
	if err != nil {
		return nil, fmt.Errorf("embeddedchat: create runtime: %w", err)
	}

	s := &Session{cfg: cfg, rt: rt}
	if root, err := loaded.Team.DefaultAgent(); err == nil {
		s.welcome = root.WelcomeMessage()
	}
	s.resetConversationLocked()
	return s, nil
}

// WelcomeMessage returns the loaded agent's welcome message.
func (s *Session) WelcomeMessage() string { return s.welcome }

// Runtime returns the underlying docker-agent runtime for advanced embedders.
// It returns nil only for sessions not created by New.
func (s *Session) Runtime() dagentruntime.Runtime {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, _ := s.rt.(dagentruntime.Runtime)
	return rt
}

// Conversation returns the underlying docker-agent session.
//
// The returned pointer is mutable and may be replaced by Restart. Callers that
// mutate it directly are responsible for coordinating with Send/Restart.
func (s *Session) Conversation() *session.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session
}

// Restart cancels any active run and replaces the conversation with a fresh
// session, preserving the runtime and loaded agent.
func (s *Session) Restart() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.cancelActiveLocked()
	s.resetConversationLocked()
	return nil
}

// Close cancels any active run and releases runtime resources.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.cancelActiveLocked()
	rt := s.rt
	s.mu.Unlock()
	if rt != nil {
		return rt.Close()
	}
	return nil
}

func (s *Session) resetConversationLocked() {
	opts := append([]session.Opt(nil), s.cfg.SessionOptions...)
	s.session = session.New(opts...)
}

func (s *Session) cancelActiveLocked() {
	if s.activeCancel != nil {
		s.activeCancel()
	}
}

// Send appends prompt to the conversation and streams the assistant reply.
// The returned channel closes when the runtime stream stops. A clean stream
// emits a final Done event first; a stream that reports an ErrorEvent emits one
// Err event, suppresses later projected events, then keeps draining until the
// runtime stops.
// If ctx is cancelled, Send drains the runtime stream until it stops, but no
// further events are delivered to the caller.
func (s *Session) Send(ctx context.Context, prompt string) (<-chan Event, error) {
	s.mu.Lock()
	if s.rt == nil || s.session == nil {
		s.mu.Unlock()
		return nil, ErrNotInitialized
	}
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	if s.activeCancel != nil {
		s.mu.Unlock()
		return nil, ErrRunActive
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.activeCancel = cancel
	s.activeRun++
	runID := s.activeRun
	s.session.AddMessage(session.UserMessage(prompt))
	events := s.rt.RunStream(runCtx, s.session)
	s.mu.Unlock()

	out := make(chan Event, eventBufferSize(s.cfg.EventBuffer))
	go s.forwardEvents(runCtx, events, out, cancel, runID)
	return out, nil
}

// Confirm answers the pending tool confirmation, if any.
func (s *Session) Confirm(ctx context.Context, req dagentruntime.ResumeRequest) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	rt := s.rt
	s.mu.Unlock()
	if rt == nil {
		return ErrNotInitialized
	}
	rt.Resume(ctx, req)
	return nil
}

func (s *Session) forwardEvents(ctx context.Context, events <-chan dagentruntime.Event, out chan<- Event, cancel context.CancelFunc, runID int) {
	defer close(out)
	defer cancel()
	defer func() {
		s.mu.Lock()
		if s.activeRun == runID {
			s.activeCancel = nil
		}
		s.mu.Unlock()
	}()

	emit := func(e Event) bool {
		select {
		case out <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}

	errSent := false
	for event := range events {
		if ctx.Err() != nil {
			continue
		}

		switch e := event.(type) {
		case *dagentruntime.ToolCallConfirmationEvent:
			if errSent {
				s.rt.Resume(ctx, dagentruntime.ResumeReject("The run was aborted."))
				continue
			}
			if !emit(Event{RuntimeEvent: event, Tool: &ToolActivity{Call: e.ToolCall, Def: e.ToolDefinition, NeedsConfirmation: true}}) {
				s.rt.Resume(ctx, dagentruntime.ResumeReject("The run was aborted."))
			}
		case *dagentruntime.ElicitationRequestEvent:
			// This headless wrapper has no built-in elicitation UI. Decline so the
			// run cannot hang forever; embedders that need elicitation can consume
			// RuntimeEvent directly by driving the runtime themselves.
			_ = s.rt.ResumeElicitation(ctx, tools.ElicitationActionDecline, nil)
		case *dagentruntime.MaxIterationsReachedEvent:
			s.rt.Resume(ctx, dagentruntime.ResumeReject(""))
		case *dagentruntime.ErrorEvent:
			if errSent {
				continue
			}
			if !emit(Event{RuntimeEvent: event, Err: errors.New(e.Error)}) {
				return
			}
			errSent = true
		default:
			if errSent {
				continue
			}
			if translated, ok := TranslateRuntimeEvent(event); ok {
				if !emit(translated) {
					return
				}
			}
		}
	}
	if !errSent && ctx.Err() == nil {
		emit(Event{Done: true})
	}
}

func eventBufferSize(configured int) int {
	if configured > 0 {
		return configured
	}
	return defaultEventBuffer
}

// TranslateRuntimeEvent translates content-bearing runtime events into the
// compact Event shape used by embedded chat UIs.
func TranslateRuntimeEvent(event dagentruntime.Event) (Event, bool) {
	switch e := event.(type) {
	case *dagentruntime.AgentChoiceEvent:
		if e.Content == "" {
			return Event{}, false
		}
		return Event{RuntimeEvent: event, Text: e.Content}, true
	case *dagentruntime.ToolCallEvent:
		return Event{RuntimeEvent: event, Tool: &ToolActivity{Call: e.ToolCall, Def: e.ToolDefinition}}, true
	case *dagentruntime.ToolCallResponseEvent:
		return Event{RuntimeEvent: event, Tool: &ToolActivity{
			Call:     tools.ToolCall{ID: e.ToolCallID},
			Def:      e.ToolDefinition,
			Finished: true,
			IsError:  e.Result != nil && e.Result.IsError,
		}}, true
	}
	return Event{}, false
}
