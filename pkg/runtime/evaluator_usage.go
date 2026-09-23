package runtime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
	"uuid"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/session"
)

// evaluatorAccounting belongs to a tool batch, never to a shared provider client.
type evaluatorAccounting struct {
	r      *LocalRuntime
	sess   *session.Session
	a      *agent.Agent
	events EventSink
	mu     sync.Mutex
	used   bool
}

type evaluatorAccountingKey struct{}

type accountedEvaluator struct {
	client evaluator.Evaluator
	name   string
}

func (e *accountedEvaluator) Evaluate(ctx context.Context, state any) (*evaluator.Result, error) {
	accounting, _ := ctx.Value(evaluatorAccountingKey{}).(*evaluatorAccounting)
	if accounting == nil {
		return e.client.Evaluate(ctx, state)
	}
	accounting.mu.Lock()
	accounting.used = true
	accounting.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if breach := accounting.r.currentBudget().exceededFor(accounting.a.Name()); breach != nil {
		return nil, errors.New(breach.Message())
	}

	started := accounting.r.now()
	var observerMu sync.Mutex
	var observed, invalid bool
	var accountedActive time.Duration
	observe := func(record evaluator.UsageRecord) {
		observerMu.Lock()
		defer observerMu.Unlock()
		observed = true
		active := max(accounting.r.now().Sub(started), accountedActive)
		if !accounting.record(ctx, e.name, record, active-accountedActive) {
			invalid = true
		}
		accountedActive = active
	}
	result, err := e.client.Evaluate(evaluator.WithUsageObserver(ctx, observe), state)
	if !observed && result != nil {
		// Custom evaluators may only implement the original Result API.
		record := evaluator.UsageRecord{Model: result.Model, Cost: result.Cost}
		if result.Usage.InputTokens != 0 || result.Usage.OutputTokens != 0 || result.Cost != nil {
			record.Usage = &result.Usage
		}
		observe(record)
	}
	if invalid {
		return nil, errors.New("evaluator reported invalid accounting")
	}
	if breach := accounting.r.currentBudget().exceededFor(accounting.a.Name()); breach != nil {
		return nil, errors.New(breach.Message())
	}
	return result, err
}

func (ac *evaluatorAccounting) record(ctx context.Context, name string, record evaluator.UsageRecord, elapsed time.Duration) bool {
	// Keep cumulative cost snapshots ordered when guards run concurrently.
	ac.mu.Lock()
	defer ac.mu.Unlock()

	valid := true
	if record.Usage != nil {
		usage := *record.Usage
		if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.InputTokens > math.MaxInt64-usage.OutputTokens {
			record.Usage, record.Cost = nil, nil
			valid = false
		} else {
			record.Usage = &usage
		}
	}
	if record.Cost != nil {
		cost := *record.Cost
		if cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) || math.IsInf(ac.sess.TotalCost()+cost, 0) {
			record.Cost = nil
			valid = false
		} else {
			record.Cost = &cost
		}
	}

	evaluation := &session.Evaluation{
		ID: uuid.NewV4().String(), Evaluator: name, AgentName: ac.a.Name(),
		Model: record.Model, Cost: record.Cost, CreatedAt: ac.r.now().Format(time.RFC3339Nano),
	}
	if record.Usage != nil {
		evaluation.Usage = &chat.Usage{InputTokens: record.Usage.InputTokens, OutputTokens: record.Usage.OutputTokens}
	}
	ac.sess.AddEvaluation(evaluation)
	ac.events.Emit(&EvaluationUsageEvent{
		Type: "evaluation_usage", SessionID: ac.sess.ID, Evaluation: evaluation,
		AgentContext: newAgentContext(ac.a.Name()),
	})
	if evaluation.Usage != nil {
		var cost float64
		if evaluation.Cost != nil {
			cost = *evaluation.Cost
		}
		ac.r.telemetry.RecordTokenUsage(ctx, record.Model, evaluation.Usage.InputTokens, evaluation.Usage.OutputTokens, cost)
	}
	ac.r.recordBudgetSpend(ac.sess, ac.a, evaluation.Usage, evaluation.Cost, elapsed, ac.events, evaluation.Cost == nil)
	if evaluation.Cost == nil {
		ac.events.Emit(Warning(fmt.Sprintf("Evaluator %q spend is unknown; session cost is incomplete. Configure evaluator cost rates when pricing is missing; requests without reported usage cannot be priced.", name), ac.a.Name()))
	}
	contextLimit := ac.r.contextLimitForAgentModel(ctx, ac.a, ac.r.getEffectiveModelID(ctx, ac.a))
	usage := SessionUsage(ac.sess, contextLimit, ac.a.CompactionThreshold())
	usage.SnapshotOnly = true
	ac.events.Emit(NewTokenUsageEvent(ac.sess.ID, ac.a.Name(), usage))
	return valid
}

// EvaluationUsageEvent reports a paid assessment independently of its decision.
type EvaluationUsageEvent struct {
	AgentContext

	Type       string              `json:"type"`
	SessionID  string              `json:"session_id"`
	Evaluation *session.Evaluation `json:"evaluation"`
}

func (e *EvaluationUsageEvent) GetSessionID() string { return e.SessionID }
