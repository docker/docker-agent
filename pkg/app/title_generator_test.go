package app

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
)

type recordingTitleGenerator struct {
	calls    int
	id       string
	messages []string
	title    string
	err      error
}

func (g *recordingTitleGenerator) Generate(_ context.Context, id string, messages []string) (string, error) {
	g.calls++
	g.id = id
	g.messages = messages
	return g.title, g.err
}

func TestTitleGeneratorContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		title string
		err   error
	}{
		{name: "success", title: "Generated title"},
		{name: "empty"},
		{name: "failure", err: errors.New("generation failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				sess := session.New()
				sess.AddMessage(session.UserMessage("hello"))
				gen := &recordingTitleGenerator{title: tc.title, err: tc.err}
				app := New(t.Context(), &mockRuntime{}, sess, WithTitleGenerator(gen))
				assert.Zero(t, gen.calls)
				require.NoError(t, app.RegenerateSessionTitle(t.Context()))
				synctest.Wait()
				assert.Equal(t, 1, gen.calls)
				assert.Equal(t, sess.ID, gen.id)
				assert.Equal(t, []string{"hello"}, gen.messages)
				assert.False(t, app.IsTitleGenerating())
				event, ok := unwrappedTestEvent(<-app.events).(*runtime.SessionTitleEvent)
				require.True(t, ok)
				assert.Equal(t, sess.ID, event.SessionID)
				want := tc.title
				if tc.err != nil {
					want = ""
				}
				assert.Equal(t, want, sess.TitleSnapshot())
				assert.Equal(t, want, event.Title)
			})
		})
	}
}

func TestNilTitleGenerator(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		gen  titleGenerator
	}{
		{"nil", nil}, {"typed nil", (*sessiontitle.Generator)(nil)}, {"no providers", sessiontitle.New()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(t.Context(), &mockRuntime{}, session.New(), WithTitleGenerator(tc.gen))
			assert.Nil(t, app.titleGen)
			require.EqualError(t, app.RegenerateSessionTitle(t.Context()), "title regeneration not available")
			assert.False(t, app.IsTitleGenerating())
		})
	}
}
