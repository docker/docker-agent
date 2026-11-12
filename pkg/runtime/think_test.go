package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/baggage"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/think"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
	"github.com/docker/docker-agent/pkg/tools/codemode"
)

func thinkStream(thought string) chat.MessageStream {
	return newStreamBuilder().AddToolCallName("think-call", "think").
		AddToolCallArguments("think-call", fmt.Sprintf(`{"thought":%q}`, thought)).AddToolCallStopWithUsage(1, 1).Build()
}

func thinkStop() chat.MessageStream {
	return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build()
}

func thinkRuntime(t *testing.T, tm *team.Team, store session.Store, current string) *LocalRuntime {
	t.Helper()
	rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent(current), WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	return rt
}

func drainThink(t *testing.T, rt *LocalRuntime, sess *session.Session) []string {
	t.Helper()
	var outputs []string
	for ev := range rt.RunStream(t.Context(), sess) {
		switch ev := ev.(type) {
		case *ToolCallResponseEvent:
			if ev.ToolDefinition.Name == "think" {
				assert.False(t, ev.Result.IsError, ev.Response)
				outputs = append(outputs, ev.Response)
			}
		case *ErrorEvent:
			t.Errorf("runtime error: %v", ev)
		}
	}
	return outputs
}

func TestThinkConversationIsolationAfterClose(t *testing.T) {
	t.Parallel()
	prov := &queueProvider{streams: []chat.MessageStream{thinkStream("first"), thinkStop(), thinkStream("second"), thinkStop()}}
	root := agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(think.New()))
	tm := team.New(team.WithAgents(root))
	store := session.NewInMemorySessionStore()
	rt := thinkRuntime(t, tm, store, "root")
	one := session.New(session.WithUserMessage("start"))
	one.ToolsApproved = true
	assert.Equal(t, []string{"Thoughts:\nfirst"}, drainThink(t, rt, one))
	require.NoError(t, rt.Close())
	rt = thinkRuntime(t, tm, store, "root")
	two := session.New(session.WithUserMessage("start"))
	two.ToolsApproved = true
	assert.Equal(t, []string{"Thoughts:\nsecond"}, drainThink(t, rt, two))
	require.NoError(t, rt.Close())
}

func TestThinkRetentionReloadCloneFork(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "session.db")
			store := session.NewInMemorySessionStore()
			if kind == "sqlite" {
				var err error
				store, err = sqlitestore.New(t.Context(), path)
				require.NoError(t, err)
			}
			prov := &queueProvider{streams: []chat.MessageStream{
				thinkStream("first"), thinkStop(), thinkStream("second"), thinkStop(),
				thinkStream("third"), thinkStop(), thinkStream("fork"), thinkStop(),
			}}
			root := agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(think.New()))
			tm := team.New(team.WithAgents(root))
			rt := thinkRuntime(t, tm, store, "root")
			sess := session.New(session.WithUserMessage("start"))
			sess.ToolsApproved = true
			stale := sess.Clone()
			stale.Origin = sess.Origin
			assert.Equal(t, []string{"Thoughts:\nfirst"}, drainThink(t, rt, sess))
			assert.Equal(t, []string{"Thoughts:\nfirst\nsecond"}, drainThink(t, rt, sess))
			require.NoError(t, rt.Close())
			require.NoError(t, store.UpdateSession(t.Context(), stale))
			if kind == "sqlite" {
				require.NoError(t, store.Close())
				var err error
				store, err = sqlitestore.New(t.Context(), path)
				require.NoError(t, err)
			}
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			reloaded, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			reloaded.ToolsApproved = true
			rt = thinkRuntime(t, tm, store, "root")
			clone := reloaded.Clone()
			assert.Equal(t, []string{"Thoughts:\nfirst\nsecond\nthird"}, drainThink(t, rt, clone))
			fork, err := session.ForkSession(clone, 0)
			require.NoError(t, err)
			fork.ToolsApproved = true
			assert.Equal(t, []string{"Thoughts:\nfork"}, drainThink(t, rt, fork))
			require.NoError(t, rt.Close())
		})
	}
}

func TestThinkParallelBatchAndAgents(t *testing.T) {
	t.Parallel()
	batch := newStreamBuilder().AddToolCallName("a", "think").AddToolCallArguments("a", `{"thought":"a"}`).
		AddToolCallName("b", "think").AddToolCallArguments("b", `{"thought":"b"}`).AddToolCallStopWithUsage(1, 1).Build()
	oneProvider := &queueProvider{streams: []chat.MessageStream{batch, thinkStop(), thinkStream("final"), thinkStop()}}
	twoProvider := &queueProvider{streams: []chat.MessageStream{thinkStream("worker"), thinkStop()}}
	shared := think.New()
	one := agent.New("root", "test", agent.WithModel(oneProvider), agent.WithToolSets(shared))
	two := agent.New("worker", "test", agent.WithModel(twoProvider), agent.WithToolSets(shared))
	tm := team.New(team.WithAgents(one, two))
	store := session.NewInMemorySessionStore()
	rt := thinkRuntime(t, tm, store, "root")
	sess := session.New(session.WithUserMessage("start"))
	sess.ToolsApproved = true
	outputs := drainThink(t, rt, sess)
	require.Len(t, outputs, 2)
	assert.ElementsMatch(t, []int{2, 3}, []int{len(strings.Split(outputs[0], "\n")), len(strings.Split(outputs[1], "\n"))})
	outputs = drainThink(t, rt, sess)
	require.Len(t, outputs, 1)
	lines := strings.Split(outputs[0], "\n")
	require.Len(t, lines, 4)
	assert.ElementsMatch(t, []string{"a", "b"}, lines[1:3])
	assert.Equal(t, "final", lines[3])
	require.NoError(t, rt.Close())
	rt = thinkRuntime(t, tm, store, "worker")
	sess.AgentName = "worker"
	assert.Equal(t, []string{"Thoughts:\nworker"}, drainThink(t, rt, sess))
	require.NoError(t, rt.Close())
}

func dispatchThink(t *testing.T, rt *LocalRuntime, ctx context.Context, sess *session.Session, caller *agent.Agent, ts tools.ToolSet, thought string) *tools.ToolCallResult {
	t.Helper()
	available, err := ts.Tools(t.Context())
	require.NoError(t, err)
	events := make(chan Event, 64)
	rt.processToolCalls(ctx, sess, caller, []tools.ToolCall{{ID: "call", Type: "function", Function: tools.FunctionCall{Name: "think", Arguments: fmt.Sprintf(`{"thought":%q}`, thought)}}}, available, NewChannelSink(events))
	close(events)
	var result *tools.ToolCallResult
	for ev := range events {
		if ev, ok := ev.(*ToolCallResponseEvent); ok {
			result = ev.Result
		}
	}
	require.NotNil(t, result)
	return result
}

func TestThinkInstanceExtraAndCallerBinding(t *testing.T) {
	t.Parallel()
	first, second, extra := think.New(), think.New(), think.New()
	caller := agent.New("root", "test", agent.WithModel(&mockProvider{stream: &mockStream{}}), agent.WithToolSets(codemode.Wrap(first), second))
	other := agent.New("worker", "test", agent.WithModel(&mockProvider{stream: &mockStream{}}), agent.WithToolSets(first))
	rt := thinkRuntime(t, team.New(team.WithAgents(caller, other)), session.NewInMemorySessionStore(), "worker")
	sess := session.New(session.WithExtraToolSets([]tools.ToolSet{first, extra}))
	sess.ToolsApproved = true
	require.NoError(t, rt.sessionStore.UpdateSession(t.Context(), sess))
	assert.Equal(t, map[*think.ToolSet]string{first: "cfg:0", second: "cfg:1", extra: "extra:0"}, thinkOwners(caller, sess.ExtraToolSets))
	for _, ts := range []*think.ToolSet{first, second, extra} {
		res := dispatchThink(t, rt, t.Context(), sess, caller, ts, "one")
		assert.False(t, res.IsError)
		assert.Equal(t, "Thoughts:\none", res.Output)
	}
	res := dispatchThink(t, rt, t.Context(), sess, caller, first, "two")
	assert.Equal(t, "Thoughts:\none\ntwo", res.Output)
	res = dispatchThink(t, rt, t.Context(), sess, other, first, "worker")
	assert.Equal(t, "Thoughts:\nworker", res.Output)
	res = dispatchThink(t, rt, t.Context(), sess, caller, think.New(), "unknown")
	assert.True(t, res.IsError)
	assert.Contains(t, res.Output, "unknown")
}

func TestThinkCodeModeSharesDirectScope(t *testing.T) {
	t.Parallel()
	ts := think.New()
	wrapped := codemode.Wrap(ts)
	script, err := json.Marshal(codemode.RunToolsWithJavascriptArgs{Script: `return await think({thought: "nested"});`})
	require.NoError(t, err)
	prov := &queueProvider{streams: []chat.MessageStream{
		newStreamBuilder().AddToolCallName("outer", "run_tools_with_javascript").AddToolCallArguments("outer", string(script)).AddToolCallStopWithUsage(1, 1).Build(), thinkStop(),
	}}
	caller := agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(wrapped))
	rt := thinkRuntime(t, team.New(team.WithAgents(caller)), session.NewInMemorySessionStore(), "root")
	sess := session.New(session.WithUserMessage("start"))
	sess.ToolsApproved = true
	assert.Equal(t, []string{"Thoughts:\nnested"}, drainThink(t, rt, sess))
	res := dispatchThink(t, rt, t.Context(), sess, caller, ts, "direct")
	assert.Equal(t, "Thoughts:\nnested\ndirect", res.Output)
	require.NoError(t, rt.Close())
}

func TestThinkChildRootAndMissingAncestors(t *testing.T) {
	t.Parallel()
	ts := think.New()
	prov := &queueProvider{streams: []chat.MessageStream{thinkStream("child"), thinkStop()}}
	caller := agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(ts))
	store := session.NewInMemorySessionStore()
	rt := thinkRuntime(t, team.New(team.WithAgents(caller)), store, "root")
	root := session.New(session.WithOrigin("a2a"))
	parent := session.New()
	parent.ParentID = root.ID
	for _, sess := range []*session.Session{root, parent} {
		require.NoError(t, store.UpdateSession(t.Context(), sess))
	}
	child := session.New(session.WithUserMessage("start"))
	child.ParentID = parent.ID
	child.ToolsApproved = true
	assert.Equal(t, []string{"Thoughts:\nchild"}, drainThink(t, rt, child))
	res := dispatchThink(t, rt, t.Context(), child, caller, ts, "fallback")
	assert.False(t, res.IsError)
	assert.Equal(t, "Thoughts:\nchild\nfallback", res.Output)
	require.NoError(t, store.DeleteSession(t.Context(), root.ID))
	res = dispatchThink(t, rt, t.Context(), child, caller, ts, "missing")
	assert.True(t, res.IsError)
	require.NoError(t, store.UpdateSession(t.Context(), root))
	require.NoError(t, store.UpdateSession(t.Context(), parent))
	res = dispatchThink(t, rt, t.Context(), child, caller, ts, "after deletion")
	assert.Equal(t, "Thoughts:\nafter deletion", res.Output)
	parent.ParentID = parent.ID
	require.NoError(t, store.UpdateSession(t.Context(), parent))
	res = dispatchThink(t, rt, t.Context(), child, caller, ts, "cycle")
	assert.True(t, res.IsError)
	require.NoError(t, rt.Close())
}

type noThinkStore struct{ session.Store }

func TestThinkMissingStoreCapabilityAndRoot(t *testing.T) {
	t.Parallel()
	ts := think.New()
	caller := agent.New("root", "test", agent.WithModel(&mockProvider{stream: &mockStream{}}), agent.WithToolSets(ts))
	tm := team.New(team.WithAgents(caller))
	sess := session.New()
	sess.ToolsApproved = true
	rt := thinkRuntime(t, tm, noThinkStore{session.NewInMemorySessionStore()}, "root")
	res := dispatchThink(t, rt, t.Context(), sess, caller, ts, "unsupported")
	assert.True(t, res.IsError)
	assert.Contains(t, res.Output, "does not support")
	rt = thinkRuntime(t, tm, session.NewInMemorySessionStore(), "root")
	res = dispatchThink(t, rt, t.Context(), sess, caller, ts, "missing root")
	assert.True(t, res.IsError)
	assert.Contains(t, res.Output, "think root unavailable")
}

type renamedThink struct {
	tools.ToolSet

	name string
}

func (r renamedThink) Unwrap() tools.ToolSet { return r.ToolSet }

func (r renamedThink) Tools(ctx context.Context) ([]tools.Tool, error) {
	list, err := r.ToolSet.Tools(ctx)
	if err == nil {
		list[0].Name = r.name
	}
	return list, err
}

func TestThinkCodeModeDistinctInstances(t *testing.T) {
	t.Parallel()
	wrapped := codemode.Wrap(renamedThink{ToolSet: think.New(), name: "first_think"}, renamedThink{ToolSet: think.New(), name: "second_think"})
	script, err := json.Marshal(codemode.RunToolsWithJavascriptArgs{Script: `await first_think({thought:"one"}); await second_think({thought:"two"}); return await first_think({thought:"three"});`})
	require.NoError(t, err)
	prov := &queueProvider{streams: []chat.MessageStream{
		newStreamBuilder().AddToolCallName("outer", "run_tools_with_javascript").AddToolCallArguments("outer", string(script)).AddToolCallStopWithUsage(1, 1).Build(), thinkStop(),
	}}
	caller := agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(wrapped))
	rt := thinkRuntime(t, team.New(team.WithAgents(caller)), session.NewInMemorySessionStore(), "root")
	sess := session.New(session.WithUserMessage("start"))
	sess.ToolsApproved = true
	var results []string
	for ev := range rt.RunStream(t.Context(), sess) {
		if ev, ok := ev.(*ToolCallResponseEvent); ok && (ev.ToolDefinition.Name == "first_think" || ev.ToolDefinition.Name == "second_think") {
			assert.False(t, ev.Result.IsError, ev.Response)
			results = append(results, ev.Response)
		}
	}
	assert.Equal(t, []string{"Thoughts:\none", "Thoughts:\ntwo", "Thoughts:\none\nthree"}, results)
	require.NoError(t, rt.Close())
}

func TestThinkParallelReloadedConversations(t *testing.T) {
	t.Parallel()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	root := session.New(session.WithOrigin("a2a"), session.WithUserMessage("start"))
	root.ToolsApproved = true
	require.NoError(t, store.UpdateSession(t.Context(), root))
	shared := think.New()
	var wg sync.WaitGroup
	outputs := make([][]string, 2)
	for i := range outputs {
		loaded, err := store.GetSession(t.Context(), root.ID)
		require.NoError(t, err)
		prov := &queueProvider{streams: []chat.MessageStream{thinkStream(fmt.Sprintf("parallel-%d", i)), thinkStop()}}
		caller := agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(shared))
		rt := thinkRuntime(t, team.New(team.WithAgents(caller)), store, "root")
		wg.Go(func() { outputs[i] = drainThink(t, rt, loaded); assert.NoError(t, rt.Close()) })
	}
	wg.Wait()
	for _, output := range outputs {
		require.Len(t, output, 1)
	}
	assert.ElementsMatch(t, []int{2, 3}, []int{len(strings.Split(outputs[0][0], "\n")), len(strings.Split(outputs[1][0], "\n"))})
	loaded, err := store.GetSession(t.Context(), root.ID)
	require.NoError(t, err)
	prov := &queueProvider{streams: []chat.MessageStream{thinkStream("final"), thinkStop()}}
	caller := agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(shared))
	rt := thinkRuntime(t, team.New(team.WithAgents(caller)), store, "root")
	result := drainThink(t, rt, loaded)
	require.Len(t, result, 1)
	assert.ElementsMatch(t, []string{"parallel-0", "parallel-1", "final"}, strings.Split(result[0], "\n")[1:])
	require.NoError(t, rt.Close())
}

type thinkStartedProvider struct {
	queueProvider

	started chan struct{}
	once    sync.Once
}

func (p *thinkStartedProvider) CreateChatCompletionStream(ctx context.Context, messages []chat.Message, available []tools.Tool) (chat.MessageStream, error) {
	p.once.Do(func() { close(p.started) })
	return p.queueProvider.CreateChatCompletionStream(ctx, messages, available)
}

func TestThinkRootObserverPrecedesProducer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		prov := &thinkStartedProvider{queueProvider: queueProvider{streams: []chat.MessageStream{thinkStream("ready"), thinkStop()}}, started: make(chan struct{})}
		caller := agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(think.New()))
		rt := thinkRuntime(t, team.New(team.WithAgents(caller)), session.NewInMemorySessionStore(), "root")
		release, entered, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		WithEventObserver(&fnObserver{onStart: func(context.Context, *session.Session) { close(entered); <-release }})(rt)
		sess := session.New(session.WithUserMessage("start"))
		sess.ToolsApproved = true
		go func() { defer close(finished); assert.Equal(t, []string{"Thoughts:\nready"}, drainThink(t, rt, sess)) }()
		<-entered
		synctest.Wait()
		select {
		case <-prov.started:
			t.Error("producer started before synchronous root observers completed")
		default:
		}
		close(release)
		<-finished
		require.NoError(t, rt.Close())
	})
}

func TestThinkTelemetryBaggageDoesNotOwnState(t *testing.T) {
	t.Parallel()
	ts := think.New()
	caller := agent.New("root", "test", agent.WithModel(&mockProvider{stream: &mockStream{}}), agent.WithToolSets(ts))
	store := session.NewInMemorySessionStore()
	rt := thinkRuntime(t, team.New(team.WithAgents(caller)), store, "root")
	member, err := baggage.NewMember("gen_ai.conversation.id", "attacker-selected")
	require.NoError(t, err)
	bag, err := baggage.New(member)
	require.NoError(t, err)
	ctx := baggage.ContextWithBaggage(t.Context(), bag)
	for _, thought := range []string{"one", "two"} {
		sess := session.New()
		sess.ToolsApproved = true
		require.NoError(t, store.UpdateSession(t.Context(), sess))
		res := dispatchThink(t, rt, ctx, sess, caller, ts, thought)
		assert.False(t, res.IsError)
		assert.Equal(t, "Thoughts:\n"+thought, res.Output)
	}
}

type thinkRecordingStore struct {
	session.Store

	mu    sync.Mutex
	roots map[string]string
}

func (s *thinkRecordingStore) AppendThought(ctx context.Context, rootID string, key session.ScratchpadKey, thought string) ([]string, error) {
	out, err := s.Store.(session.ThinkScratchpadStore).AppendThought(ctx, rootID, key, thought)
	if err == nil {
		s.mu.Lock()
		s.roots[key.SessionID] = rootID
		s.mu.Unlock()
	}
	return out, err
}

func TestThinkResumedChildDelegates(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			store := session.NewInMemorySessionStore()
			if kind == "sqlite" {
				var err error
				store, err = sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
				require.NoError(t, err)
			}
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			recorded := &thinkRecordingStore{Store: store, roots: make(map[string]string)}
			shared := think.New()
			worker := agent.New("worker", "test", agent.WithModel(&queueProvider{streams: []chat.MessageStream{thinkStream("delegated"), thinkStop()}}), agent.WithToolSets(shared))
			transfer := newStreamBuilder().AddToolCallName("transfer", "transfer_task").
				AddToolCallArguments("transfer", transferToolCall("worker").Function.Arguments).AddToolCallStopWithUsage(1, 1).Build()
			caller := agent.New("root", "test", agent.WithModel(&queueProvider{streams: []chat.MessageStream{thinkStream("resumed"), transfer, thinkStop()}}),
				agent.WithToolSets(shared, transfertask.New()), agent.WithSubAgents(worker))
			rt := thinkRuntime(t, team.New(team.WithAgents(caller, worker)), recorded, "root")
			root := session.New(session.WithOrigin("a2a"))
			intermediate := session.New(session.WithParentID(root.ID))
			for _, sess := range []*session.Session{root, intermediate} {
				require.NoError(t, store.UpdateSession(t.Context(), sess))
			}
			// Resumed streams need not re-persist their own row before delegation.
			resumed := session.New(session.WithParentID(intermediate.ID), session.WithUserMessage("start"), session.WithToolsApproved(true))
			assert.Equal(t, []string{"Thoughts:\nresumed", "Thoughts:\ndelegated"}, drainThink(t, rt, resumed))
			delegated := firstSubSession(resumed)
			require.NotNil(t, delegated)
			assert.Equal(t, map[string]string{resumed.ID: root.ID, delegated.ID: root.ID}, recorded.roots)
			require.NoError(t, rt.Close())
			require.NoError(t, store.DeleteSession(t.Context(), root.ID))
			for _, id := range []string{resumed.ID, delegated.ID} {
				_, err := recorded.AppendThought(t.Context(), root.ID, session.ScratchpadKey{SessionID: id, AgentName: "root", ToolsetKey: "cfg:0"}, "deleted")
				require.ErrorIs(t, err, session.ErrScratchpadOwner)
			}
			require.NoError(t, store.UpdateSession(t.Context(), root))
			for name, sess := range map[string]*session.Session{"root": resumed, "worker": delegated} {
				out, err := recorded.AppendThought(t.Context(), root.ID, session.ScratchpadKey{SessionID: sess.ID, AgentName: name, ToolsetKey: "cfg:0"}, "after deletion")
				require.NoError(t, err)
				assert.Equal(t, []string{"after deletion"}, out)
			}
		})
	}
}

type thinkOverlappingProvider struct {
	queueProvider

	mu      sync.Mutex
	calls   map[string]int
	arrived [2]chan string
	release [2]chan struct{}
}

func (p *thinkOverlappingProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	id := httpclient.SessionIDFromContext(ctx)
	p.mu.Lock()
	call := p.calls[id]
	p.calls[id]++
	p.mu.Unlock()
	if call >= 2 {
		return thinkStop(), nil
	}
	p.arrived[call] <- id
	select {
	case <-p.release[call]:
		return thinkStream(fmt.Sprintf("%s:%d", id, call)), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestThinkOverlappingConversationIsolation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			store := session.NewInMemorySessionStore()
			if kind == "sqlite" {
				var err error
				store, err = sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
				require.NoError(t, err)
			}
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			recorded := &thinkRecordingStore{Store: store, roots: make(map[string]string)}
			prov := &thinkOverlappingProvider{calls: make(map[string]int)}
			for i := range prov.arrived {
				prov.arrived[i] = make(chan string, 2)
				prov.release[i] = make(chan struct{})
			}
			shared := think.New()
			available, err := shared.Tools(t.Context())
			require.NoError(t, err)
			// Static advertisement avoids collectTools' concurrent TryIsStarted skips.
			caller := agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(shared), agent.WithTools(available...))
			tm := team.New(team.WithAgents(caller))
			sessions := []*session.Session{
				session.New(session.WithUserMessage("one"), session.WithToolsApproved(true)),
				session.New(session.WithUserMessage("two"), session.WithToolsApproved(true)),
			}
			var wg sync.WaitGroup
			outputs := make([][]string, len(sessions))
			for i, sess := range sessions {
				rt := thinkRuntime(t, tm, recorded, "root")
				wg.Go(func() { outputs[i] = drainThink(t, rt, sess); assert.NoError(t, rt.Close()) })
			}
			for i := range prov.arrived {
				assert.ElementsMatch(t, []string{sessions[0].ID, sessions[1].ID}, []string{<-prov.arrived[i], <-prov.arrived[i]})
				close(prov.release[i])
			}
			wg.Wait()
			for i, sess := range sessions {
				assert.Equal(t, []string{"Thoughts:\n" + sess.ID + ":0", "Thoughts:\n" + sess.ID + ":0\n" + sess.ID + ":1"}, outputs[i])
				out, err := recorded.AppendThought(t.Context(), sess.ID, session.ScratchpadKey{SessionID: sess.ID, AgentName: caller.Name(), ToolsetKey: "cfg:0"}, "verify")
				require.NoError(t, err)
				assert.Equal(t, []string{sess.ID + ":0", sess.ID + ":1", "verify"}, out)
			}
			assert.Equal(t, map[string]string{sessions[0].ID: sessions[0].ID, sessions[1].ID: sessions[1].ID}, recorded.roots)
		})
	}
}

func TestThinkRootIgnoresInheritedCandidate(t *testing.T) {
	t.Parallel()
	ts := think.New()
	caller := agent.New("root", "test", agent.WithModel(&mockProvider{stream: &mockStream{}}), agent.WithToolSets(ts))
	store := session.NewInMemorySessionStore()
	rt := thinkRuntime(t, team.New(team.WithAgents(caller)), store, "root")
	root, other := session.New(session.WithToolsApproved(true)), session.New()
	for _, sess := range []*session.Session{root, other} {
		require.NoError(t, store.UpdateSession(t.Context(), sess))
	}
	ctx := context.WithValue(t.Context(), scratchpadRootKey{}, other.ID)
	res := dispatchThink(t, rt, ctx, root, caller, ts, "own root")
	assert.False(t, res.IsError, res.Output)
	require.NoError(t, store.DeleteSession(t.Context(), other.ID))
	res = dispatchThink(t, rt, t.Context(), root, caller, ts, "retained")
	assert.False(t, res.IsError, res.Output)
	assert.Equal(t, "Thoughts:\nown root\nretained", res.Output)
	require.NoError(t, rt.Close())
}
