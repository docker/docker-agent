package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// compactionController belongs to one stream. Its methods run only on its
// owner goroutine, at boundaries where no model or tool call is in flight.
type compactionController struct {
	runtime             *LocalRuntime
	session             *session.Session
	events              EventSink
	backgroundThreshold float64
	job                 *backgroundCompaction
}

func newCompactionController(r *LocalRuntime, sess *session.Session, events EventSink, settings *userconfig.Settings) *compactionController {
	c := &compactionController{runtime: r, session: sess, events: events}
	if settings != nil && settings.BackgroundCompaction {
		c.backgroundThreshold = float64(settings.GetBackgroundCompactionThreshold()) / 100
	}
	return c
}

func (c *compactionController) BeforeTurn(ctx context.Context, a *agent.Agent, limit int64) {
	c.check(ctx, a, limit, 0)
}

func (c *compactionController) AfterMessages(ctx context.Context, a *agent.Agent, limit int64, messageCountBefore int) {
	c.check(ctx, a, limit, addedContextTokens(c.session, messageCountBefore))
}

func (c *compactionController) check(ctx context.Context, a *agent.Agent, limit, added int64) {
	if c.collect(ctx, false) {
		added = 0
	}
	if !c.runtime.sessionCompactionEnabled(a) || limit <= 0 || ctx.Err() != nil {
		return
	}
	input, output := c.session.Usage()
	if compaction.ShouldCompact(input, output, added, limit, a.CompactionThreshold()) {
		if c.collect(ctx, true) {
			added = 0
		}
		input, output = c.session.Usage()
		if ctx.Err() == nil && compaction.ShouldCompact(input, output, added, limit, a.CompactionThreshold()) {
			if err := c.runtime.compactWithReason(ctx, c.session, "", compactionReasonThreshold, c.events); err != nil {
				c.events.Emit(Warning(err.Error(), a.Name()))
			}
		}
		return
	}
	if c.backgroundThreshold > 0 && c.job == nil && !hasPendingCompactionTools(c.session) && compaction.ShouldCompact(input, output, added, limit, c.backgroundThreshold) {
		c.job = c.runtime.startBackgroundCompaction(ctx, c.session, a)
	}
}

// collect returns true when applying a summary replaced the usage estimate.
func (c *compactionController) collect(ctx context.Context, wait bool) bool {
	if c.job == nil {
		return false
	}
	if wait {
		c.job.wait(ctx, c.runtime, c.session, c.events)
	} else {
		c.job.poll(ctx, c.runtime, c.session, c.events)
	}
	if c.job.done != nil {
		return false
	}
	applied := c.job.applied
	c.job = nil
	return applied
}

func (c *compactionController) Close(ctx context.Context) {
	c.collect(ctx, false)
	if c.job != nil {
		c.job.stop(c.events)
		c.job = nil
	}
}

func addedContextTokens(sess *session.Session, messageCountBefore int) int64 {
	// Delegated sub-session content never enters this session's prompt.
	own := sess.OwnMessages()
	estimator := compaction.NewEstimator(func(yield func(*chat.Message) bool) {
		for i := range own {
			if !yield(&own[i].Message) {
				return
			}
		}
	})
	var added int64
	for i := messageCountBefore; i < len(own); i++ {
		added += estimator.EstimateMessageTokens(&own[i].Message)
	}
	return added
}
