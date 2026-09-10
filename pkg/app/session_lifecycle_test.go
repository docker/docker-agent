package app

import (
	"context"
	"io"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
)

type sessionCaptureRuntime struct {
	mockRuntime

	started chan *session.Session
	release chan struct{}
}

func (r *sessionCaptureRuntime) RunStream(_ context.Context, sess *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event)
	go func() {
		defer close(ch)
		r.started <- sess
		<-r.release
	}()
	return ch
}

func TestAppRunKeepsWorkScopedToOriginalSession(t *testing.T) {
	for _, entryPoint := range []struct {
		name string
		run  func(*App, context.Context, context.CancelFunc)
	}{
		{name: "Run", run: func(app *App, ctx context.Context, cancel context.CancelFunc) {
			app.Run(ctx, cancel, "hello", nil)
		}},
		{name: "RunWithMessage", run: func(app *App, ctx context.Context, cancel context.CancelFunc) {
			app.RunWithMessage(ctx, cancel, session.UserMessage("hello"))
		}},
	} {
		t.Run(entryPoint.name, func(t *testing.T) {
			oldSession := session.New()
			rt := &sessionCaptureRuntime{
				started: make(chan *session.Session, 1),
				release: make(chan struct{}),
			}
			app := &App{
				runtime: rt,
				session: oldSession,
				events:  make(chan tea.Msg, 4),
			}
			ctx, cancel := context.WithCancel(t.Context())
			entryPoint.run(app, ctx, cancel)

			require.Same(t, oldSession, <-rt.started)
			newSession := session.New()
			app.ReplaceSession(t.Context(), newSession)
			close(rt.release)

			event := <-app.events
			stop, ok := event.(*runtime.StreamStoppedEvent)
			require.True(t, ok)
			assert.Equal(t, oldSession.ID, stop.SessionID)
			assert.Equal(t, 1, oldSession.MessageCount())
			assert.Zero(t, newSession.MessageCount())
		})
	}
}

type signalingLocker struct {
	locked   chan struct{}
	release  chan struct{}
	unlocked chan struct{}
}

func (l *signalingLocker) Lock() {
	close(l.locked)
	<-l.release
}

func (l *signalingLocker) Unlock() {
	close(l.unlocked)
}

func TestAppRunDropsCanceledWorkWaitingForStreamGuard(t *testing.T) {
	for _, entryPoint := range []struct {
		name string
		run  func(*App, context.Context, context.CancelFunc)
	}{
		{name: "Run", run: func(app *App, ctx context.Context, cancel context.CancelFunc) {
			app.Run(ctx, cancel, "hello", nil)
		}},
		{name: "RunWithMessage", run: func(app *App, ctx context.Context, cancel context.CancelFunc) {
			app.RunWithMessage(ctx, cancel, session.UserMessage("hello"))
		}},
	} {
		t.Run(entryPoint.name, func(t *testing.T) {
			oldSession := session.New()
			guard := &signalingLocker{
				locked:   make(chan struct{}),
				release:  make(chan struct{}),
				unlocked: make(chan struct{}),
			}
			rt := &sessionCaptureRuntime{
				started: make(chan *session.Session, 1),
				release: make(chan struct{}),
			}
			app := &App{
				ctx:         func() context.Context { return t.Context() },
				runtime:     rt,
				session:     oldSession,
				events:      make(chan tea.Msg, 4),
				streamGuard: guard,
			}
			ctx, cancel := context.WithCancel(t.Context())
			entryPoint.run(app, ctx, cancel)
			<-guard.locked
			app.NewSession()
			close(guard.release)
			<-guard.unlocked

			assert.Zero(t, oldSession.MessageCount())
			assert.Zero(t, app.Session().MessageCount())
			assert.Empty(t, rt.started)
		})
	}
}

type blockingTitleProvider struct {
	started chan struct{}
	release chan struct{}
}

func (p *blockingTitleProvider) ID() modelsdev.ID {
	return modelsdev.NewID("test", "title")
}

func (p *blockingTitleProvider) BaseConfig() base.Config { return base.Config{} }

func (p *blockingTitleProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	close(p.started)
	<-p.release
	return &singleTitleStream{}, nil
}

type singleTitleStream struct {
	done bool
}

func (s *singleTitleStream) Recv() (chat.MessageStreamResponse, error) {
	if s.done {
		return chat.MessageStreamResponse{}, io.EOF
	}
	s.done = true
	return chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "Original title"}}},
	}, nil
}

func (*singleTitleStream) Close() {}

func TestGenerateTitleKeepsOriginalSession(t *testing.T) {
	provider := &blockingTitleProvider{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	oldSession := session.New()
	app := &App{
		runtime:  &mockRuntime{},
		session:  oldSession,
		events:   make(chan tea.Msg, 1),
		titleGen: sessiontitle.New(provider),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		app.generateTitle(t.Context(), oldSession, []string{"hello"})
	}()
	<-provider.started
	newSession := session.New()
	app.ReplaceSession(t.Context(), newSession)
	close(provider.release)
	<-done

	assert.Equal(t, "Original title", oldSession.TitleSnapshot())
	assert.Empty(t, newSession.TitleSnapshot())
	titleEvent, ok := (<-app.events).(*runtime.SessionTitleEvent)
	require.True(t, ok)
	assert.Equal(t, oldSession.ID, titleEvent.SessionID)
}
