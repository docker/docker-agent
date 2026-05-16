package root

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

type rootTestProvider struct{}

func (rootTestProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero("test/model") }

func (rootTestProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, nil
}

func (rootTestProvider) BaseConfig() base.Config { return base.Config{} }
func (rootTestProvider) MaxTokens() int          { return 0 }

func TestRuntimeOptsPassesRunConfigWorkingDirToRuntime(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	agt := agent.New("root", "instructions", agent.WithModel(rootTestProvider{}), agent.WithAddEnvironmentInfo(true))
	loaded := &teamloader.LoadResult{Team: team.New(team.WithAgents(agt))}
	runConfig := &config.RuntimeConfig{Config: config.Config{WorkingDir: workingDir}}

	rt, err := runtime.New(loaded.Team, (&runExecFlags{}).runtimeOpts(loaded, runConfig, session.NewInMemorySessionStore(), "root")...)
	require.NoError(t, err)

	localRt, ok := rt.(*runtime.LocalRuntime)
	require.True(t, ok)
	got := reflect.ValueOf(localRt).Elem().FieldByName("workingDir").String()

	assert.Equal(t, workingDir, got)
}
