package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/sqliteutil"
)

func NewInMemoryCheckpointStore(workflowID string) *inMemoryCheckpointStore {
	return &inMemoryCheckpointStore{
		workflowID:  workflowID,
		checkpoints: concurrent.NewMap[string, *Checkpoint](),
	}
}

func NewSQLiteCheckpointStore(ctx context.Context, workflowID string, path string) (*SQLiteCheckpointStore, error) {
	db, err := sqliteutil.OpenDB(path)
	if err != nil {
		return nil, err
	}
	// Ensure we close the connection if table creation fails
	// Note: We don't defer close here because we return the db on success

	_, err = db.ExecContext(context.Background(), "CREATE TABLE IF NOT EXISTS memories (id TEXT PRIMARY KEY, created_at TEXT, memory TEXT)")
	if err != nil {
		db.Close()
		return nil, err
	}

	return &SQLiteCheckpointStore{db: db, mu: sync.Mutex{}}, nil
}

// NewWorkflowContext returns a new WorkflowContext.
func NewWorkflowContext(workflow *Workflow, checkpointID string) (*WorkflowContext, error) {
	store, err := NewSQLiteCheckpointStore(context.Background(), checkpointID, filepath.Join(paths.GetHomeDir(), ".cagent", "session.db"))
	if err != nil {
		return nil, err
	}
	checkpoint, err := store.GetCheckpoint(context.Background(), checkpointID)
	if err != nil {
		return nil, err
	}
	memoryStore := NewInMemoryCheckpointStore(checkpointID)
	var data map[string]any
	if checkpoint != nil {
		memoryStore.checkpoints.Store(checkpointID, checkpoint)
		data = checkpoint.State
	} else {
		data = make(map[string]any)
	}

	return &WorkflowContext{
		mu:          sync.RWMutex{},
		data:        data,
		workflow:    workflow,
		store:       store,
		memoryStore: memoryStore,
	}, nil
}

// Snapshot returns a shallow copy of the internal data map for serialization/debugging.
func (c *WorkflowContext) Snapshot() map[string]any {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]any, len(c.data))
	for k, v := range c.data {
		out[k] = v
	}
	return out
}

func (s *SQLiteCheckpointStore) SaveCheckpoint(ctx context.Context, workflow *WorkflowContext) error {
	workflow.mu.RLock()
	defer workflow.mu.RUnlock()
	
	id := workflow.memoryStore.workflowID
	checkpoint := &Checkpoint{
		Name:             id,
		workflowName:     workflow.workflow.name,
		State:            workflow.data,
		CreatedAt:        time.Now(),
	}
	workflow.memoryStore.checkpoints.Store(id, checkpoint)
	
	data, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.ExecContext(ctx, "INSERT OR REPLACE INTO memories (id, created_at, memory) VALUES (?, ?, ?)", id, checkpoint.CreatedAt.Format(time.RFC3339), string(data))
	return err
}

func (s *SQLiteCheckpointStore) GetCheckpoint(ctx context.Context, id string) (*Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var createdAtStr, memoryStr string
	err := s.db.QueryRowContext(ctx, "SELECT created_at, memory FROM memories WHERE id = ?", id).Scan(&createdAtStr, &memoryStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var cp Checkpoint
	if err := json.Unmarshal([]byte(memoryStr), &cp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal checkpoint: %w", err)
	}
	return &cp, nil
}

func (s *SQLiteCheckpointStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteCheckpointStore) FlushCheckpointsToDB(workflowContext *WorkflowContext) error {
	return nil
}

// SaveCheckpoint saves the current checkpoint to the store.
func (c *WorkflowContext) SaveCheckpoint(ctx context.Context) error {
	return c.store.SaveCheckpoint(ctx, c)
}

// FlushCheckpointsToDB flushes checkpoints to the database.
func (c *WorkflowContext) FlushCheckpointsToDB() error {
	return c.store.FlushCheckpointsToDB(c)
}

// SetAgentOutput records the output of a single agent step by ID.
func (c *WorkflowContext) SetAgentOutput(stepID, output, agentName string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data == nil {
		c.data = make(map[string]any)
	}
	c.data[stepID] = StepOutput{Output: output, Agent: agentName}
}

// SetParallelOutput records the outputs of a parallel block by its step ID.
func (c *WorkflowContext) SetParallelOutput(stepID string, out *ParallelOutputs) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data == nil {
		c.data = make(map[string]any)
	}
	c.data[stepID] = out
}

// GetOutput returns the StepOutput for a step ID if it is a single agent output.
func (c *WorkflowContext) GetOutput(stepID string) (StepOutput, bool) {
	if c == nil {
		return StepOutput{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.data[stepID]
	if !ok {
		return StepOutput{}, false
	}
	so, ok := v.(StepOutput)
	return so, ok
}

// GetParallelOutput returns the ParallelOutputs for a step ID if it is a parallel block.
func (c *WorkflowContext) GetParallelOutput(stepID string) (*ParallelOutputs, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.data[stepID]
	if !ok {
		return nil, false
	}
	po, ok := v.(*ParallelOutputs)
	return po, ok
}

// EvalCondition evaluates a condition string against this context.
// Supports simple template form: {{ $steps.<id>.output }} or {{ $steps.<id>.output.path }}.
// Returns (value, true) if the expression resolves to a boolean; otherwise (nil, false).
// Full implementation would use a proper expression evaluator; this provides the contract.
func (c *WorkflowContext) EvalCondition(condition string) (bool, bool) {
	expr := strings.TrimSpace(condition)
	expr = trimTemplateBraces(expr)
	if !strings.HasPrefix(expr, "$steps.") {
		return false, false
	}
	// Minimal path: $steps.<id>.output or $steps.<id>.outputs.<stepId>.output
	parts := strings.Split(expr, ".")
	if len(parts) < 3 {
		return false, false
	}
	stepID := parts[1]
	if len(parts) >= 5 && parts[2] == "outputs" {
		// $steps.par_id.outputs.step_id.output
		parID := parts[1]
		po, ok := c.GetParallelOutput(parID)
		if !ok {
			return false, false
		}
		subID := parts[3]
		so, ok := po.Steps[subID]
		if !ok {
			return false, false
		}
		if len(parts) == 5 && parts[4] == "output" {
			return parseBool(so.Output), true
		}
		return false, false
	}
	so, ok := c.GetOutput(stepID)
	if !ok {
		return false, false
	}
	// $steps.<id>.output or $steps.<id>.output.path (e.g. is_approved)
	if len(parts) == 3 && parts[2] == "output" {
		return parseBool(so.Output), true
	}
	if len(parts) >= 4 && parts[2] == "output" {
		// Try to parse so.Output as JSON and read path (e.g. is_approved)
		var m map[string]any
		if err := json.Unmarshal([]byte(so.Output), &m); err != nil {
			return parseBool(so.Output), true
		}
		v := getPath(m, parts[3:])
		return boolFromAny(v), true
	}
	return false, false
}

func trimTemplateBraces(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "{{") {
		s = strings.TrimPrefix(s, "{{")
	}
	if strings.HasSuffix(s, "}}") {
		s = strings.TrimSuffix(s, "}}")
	}
	return strings.TrimSpace(s)
}

func getPath(m map[string]any, path []string) any {
	var v any = m
	for _, key := range path {
		if v == nil {
			return nil
		}
		mp, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v, ok = mp[key]
		if !ok {
			return nil
		}
	}
	return v
}

func parseBool(s string) bool {
	s = strings.TrimSpace(strings.ToLower(s))
	return s == "true" || s == "1" || s == "yes"
}

func boolFromAny(v any) bool {
	if v == nil {
		return false
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return parseBool(b)
	default:
		return false
	}
}
