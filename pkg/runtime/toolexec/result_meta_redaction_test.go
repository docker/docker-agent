package toolexec_test

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/portcullis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/internal/portcullistest"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	filesystemtool "github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
)

type metaEmitter struct {
	captureEmitter

	result *tools.ToolCallResult
}

func (e *metaEmitter) EmitToolCallResponse(_ string, _ tools.Tool, result *tools.ToolCallResult, _, _ string) {
	e.result = result
}

func TestDispatcherRedactsEditDiffContextBeforeEmissionAndPersistence(t *testing.T) {
	t.Parallel()
	secret := portcullistest.FakeGitHubPAT("cxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.txt")
	before := "token=" + secret + "\nsetting=old\n"
	require.NoError(t, os.WriteFile(path, []byte(before), 0o644))
	toolset := filesystemtool.New(dir)
	toolList, err := toolset.Tools(t.Context())
	require.NoError(t, err)
	reg := hooks.NewRegistry()
	require.NoError(t, builtins.Register(reg))
	cfg := &hooks.Config{ToolResponseTransform: []hooks.MatcherConfig{{Matcher: "*", Hooks: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: builtins.RedactSecrets}}}}}
	hd := &execHookDispatcher{exec: hooks.NewExecutorWithRegistry(cfg, dir, nil, reg)}
	sess := session.New()
	sess.ToolsApproved = true
	a := newAgent()
	dispatcher := &toolexec.Dispatcher{AgentFor: func(*session.Session) *agent.Agent { return a }, Hooks: hd}
	args, err := json.Marshal(filesystem.EditFileArgs{Path: "config.txt", Edits: []filesystem.Edit{{OldText: "setting=old", NewText: "setting=new"}}})
	require.NoError(t, err)
	emitter := &metaEmitter{}
	dispatcher.Process(t.Context(), sess, []tools.ToolCall{{ID: "edit", Function: tools.FunctionCall{Name: "edit_file", Arguments: string(args)}}}, toolList, emitter)
	require.NotNil(t, emitter.result)
	require.False(t, emitter.result.IsError, emitter.result.Output)
	event, err := json.Marshal(emitter.result)
	require.NoError(t, err)
	assert.NotContains(t, string(event), secret)
	assert.Contains(t, string(event), portcullis.Marker)
	stored, err := json.Marshal(sess)
	require.NoError(t, err)
	assert.NotContains(t, string(stored), secret)
	assert.Contains(t, string(stored), portcullis.Marker)
	meta, ok := filesystem.DecodeEditFileMeta(emitter.result.Meta)
	require.True(t, ok)
	require.NotEmpty(t, meta.Hunks)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "token="+secret+"\nsetting=new\n", string(after))
}

func TestDispatcherInvalidMetadataDoesNotBypassTextRedaction(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		meta any
	}{
		{"channel", make(chan int)},
		{"invalid JSON", json.RawMessage(`invalid`)},
		{"non-finite number", math.NaN()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			secret := portcullistest.FakeGitHubPAT("cxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
			reg := hooks.NewRegistry()
			require.NoError(t, builtins.Register(reg))
			cfg := &hooks.Config{ToolResponseTransform: []hooks.MatcherConfig{{Hooks: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: builtins.RedactSecrets}}}}}
			hd := &execHookDispatcher{exec: hooks.NewExecutorWithRegistry(cfg, t.TempDir(), nil, reg)}
			sess := session.New()
			sess.ToolsApproved = true
			a := newAgent()
			dispatcher := &toolexec.Dispatcher{AgentFor: func(*session.Session) *agent.Agent { return a }, Hooks: hd}
			tool := tools.Tool{Name: "report", Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
				return &tools.ToolCallResult{Output: "credential=" + secret, Meta: tt.meta}, nil
			}}
			emitter := &metaEmitter{}
			dispatcher.Process(t.Context(), sess, []tools.ToolCall{{ID: "report", Function: tools.FunctionCall{Name: "report", Arguments: `{}`}}}, []tools.Tool{tool}, emitter)
			require.NotNil(t, emitter.result)
			assert.Nil(t, emitter.result.Meta)
			assert.NotContains(t, emitter.result.Output, secret)
			assert.Contains(t, emitter.result.Output, portcullis.Marker)
			stored, err := json.Marshal(sess)
			require.NoError(t, err)
			assert.NotContains(t, string(stored), secret)
			assert.Contains(t, string(stored), portcullis.Marker)
		})
	}
}

// Keep the emitter's override checked against the runtime-facing interface.
var _ toolexec.Emitter = (*metaEmitter)(nil)
