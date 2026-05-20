package workflow

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/concurrent"
)

// StepType identifies the kind of workflow step.
type StepType string

const (
	StepTypeAgent       StepType = "agent"
	StepTypeCondition   StepType = "condition"
	StepTypeSubWorkflow StepType = "subworkflow"
	StepTypeParallel    StepType = "parallel"
)

// Step represents a single workflow step (agent, condition, or parallel block).
// Steps are defined in config and executed by the workflow executor.
type Step struct {
	// ID is a unique identifier for this step. Used for output access (e.g. $steps.<id>.output)
	// and loop detection. If empty, the executor may assign one (e.g. by index).
	ID string `json:"id,omitempty" yaml:"id,omitempty"`

	// Type is one of: agent, condition, parallel.
	Type StepType `json:"type" yaml:"type"`

	// Name is the agent name (for type=agent). Must reference an agent in config.
	Name string `json:"name,omitempty" yaml:"name,omitempty"`

	// Condition is the expression for type=condition (e.g. "{{ $steps.qa.output.is_approved }}").
	// Evaluated after referenced steps have run; must resolve to a boolean.
	Condition string `json:"condition,omitempty" yaml:"condition,omitempty"`

	// TrueSteps are executed when condition evaluates to true.
	TrueSteps []Step `json:"true,omitempty" yaml:"true,omitempty"`

	// FalseSteps are executed when condition evaluates to false.
	FalseSteps []Step `json:"false,omitempty" yaml:"false,omitempty"`

	// Steps are the child steps for type=parallel. All run concurrently.
	Steps []Step `json:"steps,omitempty" yaml:"steps,omitempty"`

	StepTimeout time.Duration `json:"stepTimeout" yaml:"stepTimeout"`

	// Workflow is the child workflow for type=subworkflow. All run concurrently.
	Workflow *Workflow `json:"workflow,omitempty" yaml:"workflow,omitempty"`

	// Retry configures per-step retry on failure.
	Retry *RetryConfig `json:"retry,omitempty" yaml:"retry,omitempty"`
}

// RetryConfig configures retry behavior for a step (agent or parallel block).
type RetryConfig struct {
	// MaxAttempts is the maximum number of attempts (including the first). Default 0 = no retry.
	MaxAttempts int `json:"max_attempts" yaml:"max_attempts"`
	// Backoff is "fixed" (constant delay) or "exponential". Optional.
	Backoff string `json:"backoff,omitempty" yaml:"backoff,omitempty"`
	// InitialDelaySeconds is the delay before first retry. Used with Backoff.
	InitialDelaySeconds int `json:"initial_delay_seconds,omitempty" yaml:"initial_delay_seconds,omitempty"`
	// On lists error patterns to retry on (e.g. ["timeout", "rate_limit"]). Empty = retry on any error.
	On []string `json:"on,omitempty" yaml:"on,omitempty"`
}

type WorkflowContext struct {
	workflow    *Workflow
	Checkpoints map[string]Checkpoint `json:"checkpoints" yaml:"checkpoints"`
	currentStep Step
	mu          sync.RWMutex
	data        map[string]any
	store       *SQLiteCheckpointStore
	memoryStore *inMemoryCheckpointStore
}

// SQLiteSessionStore implements Store using SQLite
type SQLiteCheckpointStore struct {
	mu sync.Mutex
	db *sql.DB
}

// inMemoryCheckpointStore implements Store using SQLite
type inMemoryCheckpointStore struct {
	mu          sync.Mutex
	workflowID  string
	checkpoints *concurrent.Map[string, *Checkpoint]
}

type Checkpoint struct {
	Name             string         `json:"name" yaml:"name"`
	workflowName     string         `json:"workflowName" yaml:"workflowName"`
	CurrentStepIndex int            `json:"currentStepIndex" yaml:"currentStepIndex"`
	Iteration        int            `json:"iteration" yaml:"iteration"`
	State            map[string]any `json:"state" yaml:"state"`
	CreatedAt        time.Time      `json:"createdAt" yaml:"createdAt"`
}

// Config holds workflow-level settings and the root steps.
type Workflow struct {
	name string `json:"name" yaml:"name"`

	fileName string `json:"fileName" yaml:"fileName"`

	StepTimeout time.Duration `json:"stepTimeout" yaml:"stepTimeout"`

	// Steps are the top-level workflow steps (sequential by default).
	Steps []Step `json:"steps,omitempty" yaml:"steps,omitempty"`

	// MaxLoopIterations is the maximum number of times a step can be re-executed
	// due to a conditional back-edge (loop). Default 100. Prevents infinite loops.
	MaxLoopIterations int `json:"max_loop_iterations,omitempty" yaml:"max_loop_iterations,omitempty"`
}

// UnmarshalYAML allows workflow to be specified as a list (steps only) or a map (steps + max_loop_iterations).
func (c *Workflow) UnmarshalYAML(unmarshal func(any) error) error {
	var listForm []Step
	if err := unmarshal(&listForm); err == nil {
		c.Steps = listForm
		return nil
	}
	type rawConfig Workflow
	var mapForm rawConfig
	if err := unmarshal(&mapForm); err != nil {
		return fmt.Errorf("workflow: expected a list of steps or a map with 'steps' and optional 'max_loop_iterations': %w", err)
	}
	*c = Workflow(mapForm)
	return nil
}

// DefaultMaxLoopIterations is the default cap for loop iterations when not set in config.
const DefaultMaxLoopIterations = 100

// StepOutput holds the output of a single step (e.g. last assistant message content).
type StepOutput struct {
	// Output is the last assistant message content from the step.
	Output string `json:"output"`
	// Agent is the agent name that produced this output (for type=agent).
	Agent string `json:"agent,omitempty"`
}

// ParallelOutputs is the structure passed to the next step after a parallel block.
// Keys are step IDs; order preserves deterministic indexing (e.g. outputs[0]).
type ParallelOutputs struct {
	Steps map[string]StepOutput `json:"steps"`
	Order []string              `json:"order"`
}

// GetByIndex returns the StepOutput at index i (using Order). Returns zero value if out of range.
func (p *ParallelOutputs) GetByIndex(i int) StepOutput {
	if p == nil || i < 0 || i >= len(p.Order) {
		return StepOutput{}
	}
	id := p.Order[i]
	return p.Steps[id]
}
