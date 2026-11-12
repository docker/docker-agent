package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/tools"
)

type commandRuntime struct {
	fakeRuntime

	current   string
	commands  map[string]types.Commands
	runs      int
	toolLists int
	summary   func(context.Context, *session.Session, string, runtime.EventSink)
}

func (r *commandRuntime) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{Name: r.current, Commands: r.commands[r.current]}
}
func (r *commandRuntime) CurrentAgentName(context.Context) string { return r.current }
func (r *commandRuntime) SetCurrentAgent(_ context.Context, name string) error {
	if _, ok := r.commands[name]; !ok {
		return errors.New("unknown agent")
	}
	r.current = name
	return nil
}

func (r *commandRuntime) CurrentAgentTools(context.Context) ([]tools.Tool, error) {
	r.toolLists++
	return nil, errors.New("command discovery must not start tools")
}

func (r *commandRuntime) RunStream(ctx context.Context, sess *session.Session) <-chan runtime.Event {
	r.runs++
	return r.fakeRuntime.RunStream(ctx, sess)
}

func (r *commandRuntime) Summarize(ctx context.Context, sess *session.Session, instruction string, sink runtime.EventSink) {
	if r.summary != nil {
		r.summary(ctx, sess, instruction, sink)
	}
}

func TestCommandDiscoveryIsMetadataOnly(t *testing.T) {
	t.Parallel()
	rt := &commandRuntime{current: "root", commands: map[string]types.Commands{"root": {
		"zebra": {Instruction: "literal"}, "alpha": {Description: "Alpha description", Instruction: "alpha"}, "switch": {Agent: "worker"},
		"usage": {Instruction: "collision"}, "new": {Instruction: "collision"}, "compact": {Instruction: "collision"},
		"script": {Instruction: "${shell({cmd: 'touch marker'})}"}, "bang": {Instruction: "!shell(cmd='touch marker')"}, "url": {URL: "https://example.com"},
	}}}
	commands := availableCommands(t.Context(), rt)
	var names []string
	for _, cmd := range commands {
		names = append(names, cmd.Name)
	}
	assert.Equal(t, []string{"compact", "usage", "alpha", "switch", "zebra"}, names)
	assert.Equal(t, "Alpha description", commands[2].Description)
	assert.Equal(t, "Switch to worker", commands[3].Description)
	assert.NotNil(t, commands[0].Input)
	assert.NotNil(t, commands[2].Input)
	assert.Zero(t, rt.toolLists)
	assert.Zero(t, rt.runs)
}

func TestPromptDispatchesLiteralCommandPreservingContent(t *testing.T) {
	t.Parallel()
	rt := &commandRuntime{current: "root", commands: map[string]types.Commands{"root": {"explain": {Instruction: "Explain carefully"}}}}
	a, s, peer := newPromptTestAgent(t, rt)
	request := promptRequest("")
	request.Prompt = []acpsdk.ContentBlock{acpsdk.TextBlock("/ex"), acpsdk.TextBlock("plain argument ${literal}"), acpsdk.ImageBlock(testPNGBase64(t, 1), "image/png"), acpsdk.TextBlock("after image")}
	response, err := a.Prompt(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
	assert.Equal(t, 1, rt.runs)
	messages := s.sess.OwnMessages()
	require.Len(t, messages, 1)
	assert.Equal(t, "Explain carefully argument ${literal}after image", messages[0].Message.Content)
	require.Len(t, messages[0].Message.MultiContent, 3)
	assert.Equal(t, chat.MessagePartTypeDocument, messages[0].Message.MultiContent[1].Type)
	assert.Zero(t, rt.toolLists)
	assert.Empty(t, peer.recordedReadRequests())
}

func TestCommandsNeverInterpretResourceContents(t *testing.T) {
	t.Parallel()
	rt := &commandRuntime{current: "root", commands: map[string]types.Commands{"root": {}}}
	a, s, _ := newPromptTestAgent(t, rt)
	request := promptRequest("")
	request.Prompt = []acpsdk.ContentBlock{acpsdk.ResourceBlock(acpsdk.EmbeddedResourceResource{TextResourceContents: &acpsdk.TextResourceContents{Uri: "file:///note.txt", Text: "/compact"}}), acpsdk.TextBlock("/usage")}
	_, err := a.Prompt(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, 1, rt.runs)
	assert.Equal(t, "/compact", s.sess.OwnMessages()[0].Message.MultiContent[0].Document.Source.InlineText)
}

func TestCommandsRejectUnsupportedWithoutSideEffects(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"/new", "/usage argument", "/script", "/bang", "/backtick", "/url", "/missing-agent"} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			rt := &commandRuntime{current: "root", commands: map[string]types.Commands{"root": {
				"script": {Instruction: "${shell({cmd: 'bad'})}"}, "bang": {Instruction: "!shell(cmd=bad)"}, "backtick": {Instruction: "!`bad`"}, "url": {URL: "https://example.com"}, "missing-agent": {Agent: "missing"},
			}}}
			a, s, _ := newPromptTestAgent(t, rt)
			s.sess.AddMessage(session.UserMessage("existing history"))
			_, err := a.Prompt(t.Context(), promptRequest(input))
			var rpcErr *acpsdk.RequestError
			require.ErrorAs(t, err, &rpcErr)
			assert.Equal(t, -32602, rpcErr.Code)
			assert.Zero(t, rt.runs)
			assert.Zero(t, rt.toolLists)
			assert.Equal(t, "root", rt.current)
			assert.Equal(t, []string{"existing history"}, sessionUserMessages(s.sess))
		})
	}
}

func TestBuiltinsRejectAttachmentsBeforeReading(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"/usage", "/compact"} {
		rt := &commandRuntime{current: "root", commands: map[string]types.Commands{"root": {}}}
		a, s, peer := newPromptTestAgent(t, rt)
		a.clientFS.ReadTextFile = true
		peer.readTextFile = func(acpsdk.ReadTextFileRequest) acpsdk.ReadTextFileResponse {
			t.Error("builtin must not read attachment")
			return acpsdk.ReadTextFileResponse{}
		}
		req := promptRequest(name)
		req.Prompt = append(req.Prompt, acpsdk.ResourceLinkBlock("file", "file:///file.txt"))
		_, err := a.Prompt(t.Context(), req)
		require.Error(t, err)
		assert.Zero(t, rt.runs)
		assert.Empty(t, s.sess.OwnMessages())
		assert.Empty(t, peer.recordedReadRequests())
	}
}

func TestAgentSwitchCommandUsesOriginalDefinition(t *testing.T) {
	t.Parallel()
	rt := &commandRuntime{current: "root", commands: map[string]types.Commands{
		"root":   {"switch": {Agent: "worker"}, "review": {Agent: "worker", Instruction: "Review from root"}},
		"worker": {"review": {Instruction: "Wrong definition"}},
	}}
	a, s, peer := newPromptTestAgent(t, rt)
	_, err := a.Prompt(t.Context(), promptRequest("/switch"))
	require.NoError(t, err)
	assert.Equal(t, "worker", rt.current)
	assert.Zero(t, rt.runs)
	assert.Empty(t, s.sess.OwnMessages())
	assert.Contains(t, strings.Join(peer.out.(*captureWriter).lines(), "\n"), "Wrong definition")
	rt.current = "root"
	_, err = a.Prompt(t.Context(), promptRequest("/review changes"))
	require.NoError(t, err)
	assert.Equal(t, "worker", rt.current)
	assert.Equal(t, []string{"Review from root changes"}, sessionUserMessages(s.sess))
	_, err = a.Prompt(t.Context(), promptRequest("/unknown text"))
	require.NoError(t, err)
	assert.Equal(t, []string{"Review from root changes", "/unknown text"}, sessionUserMessages(s.sess))
}

func TestUsageCommandDoesNotChangeHistory(t *testing.T) {
	t.Parallel()
	rt := &commandRuntime{current: "root", commands: map[string]types.Commands{"root": {}}}
	a, s, peer := newPromptTestAgent(t, rt)
	s.sess.SetUsage(120, 30)
	s.recordUsage(&runtime.TokenUsageEvent{SessionID: s.sess.ID, AgentContext: runtime.AgentContext{AgentName: "root"}, Usage: &runtime.Usage{ContextLimit: 1000}})
	s.recordUsage(&runtime.TokenUsageEvent{SessionID: "child", Usage: &runtime.Usage{ContextLimit: 5}})
	_, err := a.Prompt(t.Context(), promptRequest("/usage"))
	require.NoError(t, err)
	assert.Zero(t, rt.runs)
	assert.Empty(t, s.sess.OwnMessages())
	output := strings.Join(peer.out.(*captureWriter).lines(), "\n")
	assert.Contains(t, output, "150 tokens (last reported limit 1000)")
	rt.current = "worker"
	_, err = a.Prompt(t.Context(), promptRequest("/usage"))
	require.NoError(t, err)
	assert.Contains(t, strings.Join(peer.out.(*captureWriter).lines(), "\n"), "context limit unknown")
}

func TestCompactCommandOutcomes(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{runtime.CompactionOutcomeApplied, runtime.CompactionOutcomeSkipped, runtime.CompactionOutcomeFailed, "veto", "error"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			rt := &commandRuntime{current: "root", commands: map[string]types.Commands{"root": {}}}
			a, s, peer := newPromptTestAgent(t, rt)
			s.sess.AddMessage(session.UserMessage("history"))
			rt.summary = func(ctx context.Context, sess *session.Session, instruction string, sink runtime.EventSink) {
				assert.Equal(t, "focus on changes", instruction)
				sid, ok := getSessionID(ctx)
				assert.True(t, ok)
				assert.Equal(t, s.id, sid)
				sink.Emit(runtime.ErrorForSession("child", "not root failure"))
				switch outcome {
				case "veto":
					return
				case "error":
					sink.Emit(runtime.ErrorForSession(sess.ID, "compaction failed"))
				default:
					sink.Emit(runtime.SessionCompactionCompleted(sess.ID, outcome, "root"))
				}
			}
			response, err := a.Prompt(t.Context(), promptRequest("/compact focus on changes"))
			if outcome == runtime.CompactionOutcomeFailed || outcome == "error" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
				want := "Compaction skipped."
				if outcome == runtime.CompactionOutcomeApplied {
					want = "Session history compacted."
				}
				assert.Contains(t, strings.Join(peer.out.(*captureWriter).lines(), "\n"), want)
			}
			assert.Zero(t, rt.runs)
			assert.Equal(t, []string{"history"}, sessionUserMessages(s.sess))
		})
	}
}

func TestCompactCommandDrainsAfterCancelOrSendFailure(t *testing.T) {
	t.Parallel()
	for _, sendFail := range []bool{false, true} {
		t.Run(strconv.FormatBool(sendFail), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rt := &commandRuntime{current: "root", commands: map[string]types.Commands{"root": {}}}
				a, _, peer := newPromptTestAgent(t, rt)
				started, release := make(chan struct{}), make(chan struct{})
				if sendFail {
					peer.out.(*captureWriter).failOn = func(n int) error {
						if n > 1 {
							return io.ErrClosedPipe
						}
						return nil
					}
				}
				rt.summary = func(ctx context.Context, _ *session.Session, _ string, sink runtime.EventSink) {
					close(started)
					if sendFail {
						sink.Emit(runtime.Warning("notice", "root"))
					}
					<-ctx.Done()
					<-release
				}
				done := promptAsync(a, t.Context(), promptRequest("/compact"))
				<-started
				if !sendFail {
					require.NoError(t, a.Cancel(t.Context(), acpsdk.CancelNotification{SessionId: testSessionID}))
				}
				synctest.Wait()
				select {
				case <-done:
					t.Fatal("returned before summarize finished")
				default:
				}
				close(release)
				result := <-done
				if sendFail {
					require.ErrorContains(t, result.err, io.ErrClosedPipe.Error())
				} else {
					require.NoError(t, result.err)
					assert.Equal(t, acpsdk.StopReasonCancelled, result.response.StopReason)
				}
			})
		})
	}
}

type commandProbeToolset struct{ calls atomic.Int32 }

func (t *commandProbeToolset) Tools(context.Context) ([]tools.Tool, error) {
	t.calls.Add(1)
	return nil, nil
}
func (t *commandProbeToolset) Start(context.Context) error { t.calls.Add(1); return nil }
func (*commandProbeToolset) Stop(context.Context) error    { return nil }

func TestCommandsAdvertisedOnSessionSetupWithoutTools(t *testing.T) {
	t.Parallel()
	a := NewAgent(nil, &config.RuntimeConfig{}, session.NewInMemorySessionStore())
	a.team = team.New()
	probe := &commandProbeToolset{}
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		root := agent.New("root", "test", agent.WithModel(&mockProvider{id: modelsdev.NewID("test", "commands")}), agent.WithToolSets(probe), agent.WithCommands(types.Commands{"explain": {Instruction: "Explain"}, "danger": {Instruction: "${shell({cmd: 'bad'})}"}}))
		return &teamloader.LoadResult{Team: team.New(team.WithAgents(root))}, nil
	}
	f := newRunAgentFixture(t, &fakeRuntime{}, &captureWriter{})
	a.SetAgentConnection(f.agent.conn)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	wd := t.TempDir()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd})
	require.NoError(t, err)
	assert.Zero(t, probe.calls.Load())
	require.Len(t, f.out.lines(), 1)
	assert.Contains(t, f.out.lines()[0], "explain")
	assert.NotContains(t, f.out.lines()[0], "danger")
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd})
	require.NoError(t, err)
	require.Len(t, f.out.lines(), 2)
	_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd})
	require.NoError(t, err)
	require.Len(t, f.out.lines(), 3)
	assert.Zero(t, probe.calls.Load())
}

func TestCommandUnicodeWhitespace(t *testing.T) {
	t.Parallel()
	for _, separator := range []string{"\u00a0", "\u2003", "\t", "\n"} {
		rt := &commandRuntime{current: "root", commands: map[string]types.Commands{"root": {"explain": {Instruction: "Explain"}, "bad" + separator + "name": {Instruction: "hidden"}}}}
		a, s, _ := newPromptTestAgent(t, rt)
		_, err := a.Prompt(t.Context(), promptRequest("/explain"+separator+"argument"))
		require.NoError(t, err)
		assert.Equal(t, []string{"Explain argument"}, sessionUserMessages(s.sess))
		_, err = a.Prompt(t.Context(), promptRequest("/usage"+separator))
		require.NoError(t, err)
		assert.Len(t, availableCommands(t.Context(), rt), 3)
	}
}

type nativeCommandCompactor struct {
	mockProvider

	seen           []chat.Message
	instructions   string
	conversationID string
	httpSessionID  string
}

func (p *nativeCommandCompactor) BaseConfig() base.Config {
	return base.Config{ModelConfig: latest.ModelConfig{ProviderOpts: map[string]any{"native_compaction": true, "context_size": 1000}}}
}

func (p *nativeCommandCompactor) CompactConversation(ctx context.Context, messages []chat.Message, _ []tools.Tool, instructions string) (*chat.CompactionResult, error) {
	p.seen = messages
	p.instructions = instructions
	p.conversationID = genai.ConversationIDFromContext(ctx)
	p.httpSessionID = httpclient.SessionIDFromContext(ctx)
	return &chat.CompactionResult{Summary: "persisted summary", Provider: "test", Block: json.RawMessage(`{"summary":"persisted summary"}`)}, nil
}

type failingCompactStore struct{ session.Store }

func (failingCompactStore) PersistCompaction(context.Context, *session.Session, int64, int64, session.Item) error {
	return errors.New("persistence failed")
}

func TestCompactCommandPersistsAndUsesRuntimeScope(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			t.Parallel()
			store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "session.db"))
			require.NoError(t, err)
			defer func() { require.NoError(t, store.Close()) }()
			a := NewAgent(nil, &config.RuntimeConfig{}, store)
			if fail {
				a.sessionStore = failingCompactStore{store}
			}
			a.team = team.New()
			var compactors []*nativeCommandCompactor
			outcomes := make(chan elicitationOutcome, 2)
			a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
				p := &nativeCommandCompactor{mockProvider: mockProvider{id: modelsdev.NewID("test", "compact")}}
				compactors = append(compactors, p)
				probe := &acpElicitingToolset{atStart: true, request: &mcp.ElicitParams{Message: "startup input", Mode: "form"}, outcomes: outcomes}
				root := agent.New("root", "test", agent.WithModel(p), agent.WithToolSets(probe))
				return &teamloader.LoadResult{Team: team.New(team.WithAgents(root))}, nil
			}
			f := newRunAgentFixture(t, &fakeRuntime{}, &captureWriter{})
			a.SetAgentConnection(f.agent.conn)
			defer func() { require.NoError(t, a.Stop(t.Context())) }()
			wd := t.TempDir()
			created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd})
			require.NoError(t, err)
			s := a.sessions[string(created.SessionId)]
			message := session.UserMessage("history to compact")
			s.sess.AddMessage(message)
			_, err = store.AddMessage(t.Context(), s.id, message)
			require.NoError(t, err)
			for turn := range 2 {
				if turn == 1 {
					_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
					require.NoError(t, err)
					_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd})
					require.NoError(t, err)
				}
				_, err := a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("/compact focus on changes")}})
				if fail {
					require.ErrorContains(t, err, "persistence failed")
				} else {
					require.NoError(t, err)
				}
				select {
				case outcome := <-outcomes:
					require.NoError(t, outcome.err)
					assert.Equal(t, tools.ElicitationActionDecline, outcome.result.Action)
				default:
					t.Fatal("native compaction must install a request-scoped handler before starting tools")
				}
				assert.Equal(t, "focus on changes", compactors[turn].instructions)
				assert.Equal(t, s.id, compactors[turn].conversationID)
				assert.Equal(t, s.id, compactors[turn].httpSessionID)
				persisted, err := store.GetSession(t.Context(), s.id)
				require.NoError(t, err)
				var count int
				for _, item := range persisted.Messages {
					if item.Summary != "" {
						count++
					}
					if item.Message != nil {
						assert.NotContains(t, item.Message.Message.Content, "/compact")
					}
				}
				if fail {
					assert.Zero(t, count)
					assert.Empty(t, persisted.LastSummary())
				} else {
					assert.Equal(t, turn+1, count, "exactly one persisted summary per invocation")
					assert.Equal(t, "persisted summary", persisted.LastSummary())
					if turn == 1 {
						assert.Contains(t, fmt.Sprint(compactors[turn].seen), "persisted summary")
					}
				}
			}
		})
	}
}

type orderedCommandRuntime struct {
	fakeRuntime

	calls        atomic.Int32
	firstEntered chan struct{}
	releaseFirst chan struct{}
}

func (r *orderedCommandRuntime) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	if r.calls.Add(1) == 1 {
		close(r.firstEntered)
		<-r.releaseFirst
		return runtime.CurrentAgentInfo{Commands: types.Commands{"old": {Instruction: "old"}}}
	}
	return runtime.CurrentAgentInfo{Commands: types.Commands{"newer": {Instruction: "newer"}}}
}

func TestCommandAdvertisementsCannotOvertakeEachOther(t *testing.T) {
	t.Parallel()
	rt := &orderedCommandRuntime{firstEntered: make(chan struct{}), releaseFirst: make(chan struct{})}
	a, s, peer := newPromptTestAgent(t, rt)
	done := make(chan error, 2)
	go func() { done <- a.emitAvailableCommands(t.Context(), s) }()
	<-rt.firstEntered
	started := make(chan struct{})
	go func() { close(started); done <- a.emitAvailableCommands(t.Context(), s) }()
	<-started
	close(rt.releaseFirst)
	require.NoError(t, <-done)
	require.NoError(t, <-done)
	lines := peer.out.(*captureWriter).lines()
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], `"name":"old"`)
	assert.Contains(t, lines[1], `"name":"newer"`)
}

func TestCommandDiscoveryFailureDoesNotFailSessionSetup(t *testing.T) {
	t.Parallel()
	a := NewAgent(nil, &config.RuntimeConfig{}, session.NewInMemorySessionStore())
	a.team = team.New()
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		root := agent.New("root", "test", agent.WithModel(&mockProvider{id: modelsdev.NewID("test", "discovery")}))
		return &teamloader.LoadResult{Team: team.New(team.WithAgents(root))}, nil
	}
	out := &captureWriter{failOn: func(int) error { return io.ErrClosedPipe }}
	f := newRunAgentFixture(t, &fakeRuntime{}, out)
	a.SetAgentConnection(f.agent.conn)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	wd := t.TempDir()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd})
	require.NoError(t, err)
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd})
	require.NoError(t, err)
	_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	require.NotNil(t, s)
	assert.Empty(t, s.sess.OwnMessages())
	assert.Empty(t, out.lines())
}
