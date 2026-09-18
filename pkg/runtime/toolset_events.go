package runtime

import (
	"reflect"

	ragtypes "github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// subscribeToolsetEvents subscribes this stream's event sink to the
// background events of every toolset reachable from the team (all agents
// plus the session's skill-provided extra toolsets) and returns a release
// function that unsubscribes them all. Today that is RAG lifecycle events
// (indexing progress, usage, errors); tools-changed notifications are
// runtime-scoped and handled by OnToolsChanged. Like subscribePlanChanges it
// is called once per stream — before the toolsets start, so indexing kicked
// off by this stream's getTools is not lost — and released before the events
// channel closes.
//
// Events are attributed to the agent owning the toolset, resolved here rather
// than at event time, so attribution stays stable while the runtime's current
// agent changes mid-stream. Extra toolsets belong to the session's agent.
// A toolset instance reachable from several agents is subscribed once.
//
// Non-blocking sink: the RAG watcher outlives the per-message events channel,
// and a blocking send after the channel closes would panic.
func (r *LocalRuntime) subscribeToolsetEvents(sess *session.Session, events EventSink) (release func()) {
	sink := nonBlocking(events)

	type ownedToolSet struct {
		ts    tools.ToolSet
		agent string
	}
	var toolsets []ownedToolSet
	sessionAgent := r.resolveSessionAgent(sess).Name()
	for _, ts := range sess.ExtraToolSets {
		toolsets = append(toolsets, ownedToolSet{ts: ts, agent: sessionAgent})
	}
	for _, name := range r.team.AgentNames() {
		a, err := r.team.Agent(name)
		if err != nil {
			continue
		}
		for _, ts := range a.ToolSets() {
			toolsets = append(toolsets, ownedToolSet{ts: ts, agent: a.Name()})
		}
	}

	seen := make(map[notifierIdentity]struct{})
	var unsubs []func()
	for _, owned := range toolsets {
		for _, ragTool := range tools.FindAll[ragtypes.EventSubscriber](owned.ts) {
			if key, identifiable := pointerIdentity(ragTool); identifiable {
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
			}
			owner := owned.agent
			forward := forwardRAGEvents(ragTool.Name(), func() string { return owner }, sink.Emit)
			unsubs = append(unsubs, ragTool.SubscribeEvents(forward))
		}
	}
	return func() {
		for _, unsub := range unsubs {
			unsub()
		}
	}
}

// pointerIdentity derives a dedup key for v, reporting ok=false for
// non-pointer implementations, which are then subscribed without dedup.
// See notifierDedupKey for why pointer identity is the only hash-safe choice.
func pointerIdentity(v any) (notifierIdentity, bool) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer {
		return notifierIdentity{}, false
	}
	return notifierIdentity{typ: rv.Type(), ptr: rv.Pointer()}, true
}

// subscribeToolsChanged subscribes emitToolsChanged to every tools-changed
// notifier reachable from the team's toolsets and returns the unsubscribe
// functions. ChangeSubscriber toolsets get a subscription of their own, so
// several runtimes sharing a toolset all stay notified; toolsets that only
// implement ChangeNotifier fall back to the single legacy slot.
func (r *LocalRuntime) subscribeToolsChanged() []func() {
	var unsubs []func()
	for _, name := range r.team.AgentNames() {
		a, err := r.team.Agent(name)
		if err != nil {
			continue
		}
		for _, ts := range a.ToolSets() {
			tools.Walk(ts, func(ts tools.ToolSet) bool {
				switch n := ts.(type) {
				case tools.ChangeSubscriber:
					unsubs = append(unsubs, n.SubscribeToolsChanged(r.emitToolsChanged))
				case tools.ChangeNotifier:
					n.SetToolsChangedHandler(r.emitToolsChanged)
				}
				return true
			})
		}
	}
	return unsubs
}
