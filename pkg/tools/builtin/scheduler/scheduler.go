package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameCreateSchedule = "create_schedule"
	ToolNameListSchedules  = "list_schedules"
	ToolNameCancelSchedule = "cancel_schedule"

	category         = "scheduler"
	loopMaxWait      = time.Minute
	recallRetryDelay = time.Minute
)

func CreateToolSet() (tools.ToolSet, error) {
	return New(), nil
}

type ToolSet struct {
	store *store
	now   func() time.Time
	wake  chan struct{}

	mu     sync.Mutex
	rt     tools.Runtime
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var (
	_ tools.ToolSet      = (*ToolSet)(nil)
	_ tools.Startable    = (*ToolSet)(nil)
	_ tools.Instructable = (*ToolSet)(nil)
)

func New() *ToolSet {
	return &ToolSet{
		store: newStore(),
		now:   time.Now,
		wake:  make(chan struct{}, 1),
	}
}

type CreateScheduleArgs struct {
	Prompt string `json:"prompt" jsonschema:"The instruction to deliver to the agent when the schedule fires"`
	When   string `json:"when" jsonschema:"When to fire: in:<dur> (e.g. in:10m), at:<RFC3339> (e.g. at:2026-07-14T09:00:00Z), every:<dur> (e.g. every:1h, minimum 1m), or one of minutely, hourly, daily, weekly"`
	Name   string `json:"name,omitempty" jsonschema:"Optional human-readable label for the schedule"`
}

type ListSchedulesArgs struct{}

type CancelScheduleArgs struct {
	ID string `json:"id" jsonschema:"The id of the schedule to cancel (from create_schedule or list_schedules)"`
}

func (t *ToolSet) createSchedule(_ context.Context, args CreateScheduleArgs, rt tools.Runtime) (*tools.ToolCallResult, error) {
	if strings.TrimSpace(args.Prompt) == "" {
		return tools.ResultError("Error: prompt is required."), nil
	}
	if !rt.Supports(tools.CapabilityRecall) {
		return tools.ResultError("Error: scheduling requires host recall support, which is unavailable in this session."), nil
	}

	t.setRuntime(rt)

	sc, err := t.store.add(args.Name, args.Prompt, args.When, t.now())
	if err != nil {
		return tools.ResultError("Error: " + err.Error()), nil
	}
	t.signalWake()

	return tools.ResultSuccess(formatCreated(sc)), nil
}

func (t *ToolSet) listSchedules(_ context.Context, _ ListSchedulesArgs) (*tools.ToolCallResult, error) {
	return tools.ResultSuccess(formatList(t.store.list())), nil
}

func (t *ToolSet) cancelSchedule(_ context.Context, args CancelScheduleArgs) (*tools.ToolCallResult, error) {
	if t.store.cancel(args.ID) {
		return tools.ResultSuccess("Cancelled schedule " + args.ID + "."), nil
	}
	return tools.ResultError("Error: no schedule with id " + args.ID + "."), nil
}

func (t *ToolSet) fireDue(ctx context.Context, now time.Time) {
	rt := t.runtime()
	if rt == nil {
		return
	}
	for _, sc := range t.store.claimDue(now) {
		err := rt.Recall(ctx, formatFire(sc))
		t.store.finishFire(sc.ID, now, err == nil)
		if err != nil {
			slog.WarnContext(ctx, "Failed to enqueue scheduled recall",
				"schedule_id", sc.ID, "schedule_name", sc.Name, "error", err)
		}
	}
}

func (t *ToolSet) Start(context.Context) error {
	ctx, cancel := context.WithCancel(context.Background())
	t.mu.Lock()
	t.cancel = cancel
	t.mu.Unlock()

	t.wg.Add(1)
	go t.loop(ctx)
	return nil
}

func (t *ToolSet) Stop(context.Context) error {
	t.mu.Lock()
	cancel := t.cancel
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	t.wg.Wait()
	return nil
}

func (t *ToolSet) loop(ctx context.Context) {
	defer t.wg.Done()
	for {
		wait := loopMaxWait
		if d, ok := t.store.untilNext(t.now()); ok && d < wait {
			wait = d
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-t.wake:
			timer.Stop()
		case <-timer.C:
			t.fireDue(ctx, t.now())
		}
	}
}

func (t *ToolSet) signalWake() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *ToolSet) setRuntime(rt tools.Runtime) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rt = rt
}

func (t *ToolSet) runtime() tools.Runtime {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rt
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:                    ToolNameCreateSchedule,
			Category:                category,
			Description:             "Schedule an instruction to be delivered back to you at a specific time or on a recurring interval. When it fires you receive the instruction and act on it with your normal tools.",
			Parameters:              tools.MustSchemaFor[CreateScheduleArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewRuntimeHandler(t.createSchedule),
			Annotations:             tools.ToolAnnotations{Title: "Create Schedule"},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameListSchedules,
			Category:                category,
			Description:             "List all active schedules with their id, spec, and next fire time.",
			Parameters:              tools.MustSchemaFor[ListSchedulesArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.listSchedules),
			Annotations:             tools.ToolAnnotations{Title: "List Schedules", ReadOnlyHint: true},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameCancelSchedule,
			Category:                category,
			Description:             "Cancel a schedule by its id.",
			Parameters:              tools.MustSchemaFor[CancelScheduleArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.cancelSchedule),
			Annotations:             tools.ToolAnnotations{Title: "Cancel Schedule"},
			AddDescriptionParameter: true,
		},
	}, nil
}

func (t *ToolSet) Instructions() string {
	return `## Scheduler Tool

Use the scheduler to make something happen at a chosen time or on a repeating
cadence during this session.

- create_schedule(prompt, when, name?) registers a schedule. When it fires, its
  prompt is delivered back to you as a message and you carry out the action with
  your normal tools (shell, api, fetch, …).
- when accepts: in:<dur> (in:10m), at:<RFC3339> (at:2026-07-14T09:00:00Z),
  every:<dur> (every:1h), or minutely / hourly / daily / weekly.
- Recurring schedules must be at least 1m apart; each fire costs a turn, so
  prefer the longest cadence that still meets the need.
- list_schedules shows active schedules; cancel_schedule removes one by id.

Schedules only fire while this session is running and are not persisted across
restarts.`
}

func formatCreated(sc Schedule) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Scheduled %s: next fire at %s", label(sc), sc.NextFire.Format(time.RFC3339))
	if sc.Recurring() {
		fmt.Fprintf(&b, ", then every %s", sc.Interval)
	}
	b.WriteString(".")
	return b.String()
}

func formatList(list []Schedule) string {
	if len(list) == 0 {
		return "No active schedules."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d active schedule(s):\n", len(list))
	for _, sc := range list {
		recurring := "one-shot"
		if sc.Recurring() {
			recurring = "every " + sc.Interval.String()
		}
		fmt.Fprintf(&b, "- %s [%s] next: %s (%s)\n    %s\n",
			label(sc), sc.Spec, sc.NextFire.Format(time.RFC3339), recurring, sc.Prompt)
	}
	return b.String()
}

func formatFire(sc Schedule) string {
	return fmt.Sprintf("⏰ Scheduled task %s is due:\n%s", label(sc), sc.Prompt)
}

func label(sc Schedule) string {
	if sc.Name != "" {
		return fmt.Sprintf("%q (%s)", sc.Name, sc.ID)
	}
	return sc.ID
}
