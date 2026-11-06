package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/runtime/compactor"
	"github.com/docker/docker-agent/pkg/session"
)

// One job belongs to one stream; only its owner may apply a result.
type backgroundCompaction struct {
	cancel   context.CancelFunc
	release  func()
	done     chan struct{}
	snapshot *session.Session
	agent    *agent.Agent
	result   *compactor.Result
	applied  bool
	events   []Event
	mu       sync.Mutex
}

func (b *backgroundCompaction) Emit(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, e)
}

func (b *backgroundCompaction) flush(events EventSink) {
	b.mu.Lock()
	pending := b.events
	b.events = nil
	b.mu.Unlock()
	for _, e := range pending {
		events.Emit(e)
	}
}

func (b *backgroundCompaction) stop(events EventSink) {
	if b.done == nil {
		return
	}
	b.cancel()
	<-b.done
	b.flush(events)
	if b.result != nil {
		events.Emit(SessionCompactionCompleted(b.snapshot.ID, CompactionOutcomeSkipped, b.agent.Name()))
	}
	b.done = nil
	if b.release != nil {
		b.release()
	}
	b.result = nil
}

func (b *backgroundCompaction) wait(ctx context.Context, r *LocalRuntime, sess *session.Session, events EventSink) {
	if b.done == nil {
		return
	}
	select {
	case <-b.done:
		b.poll(ctx, r, sess, events)
	case <-ctx.Done():
		b.stop(events)
	}
}

func (b *backgroundCompaction) poll(ctx context.Context, r *LocalRuntime, sess *session.Session, events EventSink) {
	if b.done == nil {
		return
	}
	b.flush(events)
	select {
	case <-b.done:
	default:
		return
	}
	b.cancel()
	b.done = nil
	if b.release != nil {
		defer b.release()
	}
	b.flush(events)
	if b.result == nil {
		return
	}
	outcome := CompactionOutcomeSkipped
	// A manual/overflow compaction or history edit supersedes this snapshot.
	items := sess.MessagesSnapshot()
	original := b.snapshot.Messages
	valid := len(items) >= len(original) && r.resolveSessionAgent(sess) == b.agent
	if valid {
		before, err1 := json.Marshal(original)
		after, err2 := json.Marshal(items[:len(original)])
		valid = err1 == nil && err2 == nil && bytes.Equal(before, after)
	}
	if valid {
		for _, item := range items[len(original):] {
			if item.Summary != "" {
				valid = false
				break
			}
		}
	}
	if valid && ctx.Err() == nil {
		if b.result.FirstKeptEntry <= 0 || b.result.FirstKeptEntry > len(original) {
			b.result.FirstKeptEntry = len(original)
		}
		// A boundary equal to the snapshot length preserves every newly appended item.
		// Include that tail in pressure accounting rather than resetting to summary-only usage.
		for _, item := range items[b.result.FirstKeptEntry:] {
			if item.Message != nil {
				b.result.InputTokens += compaction.EstimateMessageTokens(&item.Message.Message)
			}
		}
		outcome = r.applyCompactionResult(ctx, sess, b.agent, b.result, compactionReasonThreshold, events)
		b.applied = outcome == CompactionOutcomeApplied
		modelID := r.getEffectiveModelID(ctx, b.agent)
		limit := r.effectiveContextLimit(ctx, b.agent, r.resolveContextLimit(ctx, b.agent.Model(ctx), modelID))
		events.Emit(NewTokenUsageEvent(sess.ID, b.agent.Name(), SessionUsage(sess, limit, b.agent.CompactionThreshold())))
	}
	events.Emit(SessionCompactionCompleted(sess.ID, outcome, b.agent.Name()))
	b.result = nil
}

func (r *LocalRuntime) startBackgroundCompaction(ctx context.Context, sess *session.Session, a *agent.Agent) *backgroundCompaction {
	if !r.acquireCompaction(sess.ID) {
		return nil
	}
	snapshot := sess.Clone()
	snapshot.AgentName = a.Name()
	jobCtx, cancel := context.WithCancel(ctx)
	b := &backgroundCompaction{snapshot: snapshot, agent: a, cancel: cancel, done: make(chan struct{}), release: func() { r.activeCompactions.Delete(sess.ID) }}
	done := b.done
	go func() {
		defer close(done)
		skip, msg, prompt := r.executePreCompactHooks(jobCtx, snapshot, a, preCompactSourceFor(compactionReasonThreshold), b)
		if skip {
			if msg != "" {
				b.Emit(Warning(msg, a.Name()))
			}
			return
		}
		b.result = r.generateCompaction(jobCtx, snapshot, a, prompt, compactionReasonThreshold, b)
	}()
	return b
}
