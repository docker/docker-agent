package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/memory/database"
	"github.com/docker/docker-agent/pkg/sqliteutil"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
)

// memoryDataSourceID is the `gen_ai.data_source.id` value used on
// retrieval-shaped memory operations (SearchMemories) so observability-svc
// can group "agent recalled this memory" timeline entries the same way it
// groups RAG retrievals.
const memoryDataSourceID = "memory"

// startMemorySpan opens a small INTERNAL span for a memory CRUD operation.
// op is recorded as `cagent.memory.op` and the span name is
// `memory.{op}`. Conversation id flows in via baggage so the span lands
// on the right session timeline.
func startMemorySpan(ctx context.Context, op string) (context.Context, trace.Span) {
	tracer := otel.Tracer("github.com/docker/docker-agent/pkg/memory/database/sqlite")
	attrs := []attribute.KeyValue{
		attribute.String("cagent.memory.op", op),
	}
	if convID := genai.ConversationIDFromContext(ctx); convID != "" {
		attrs = append(attrs, attribute.String(genai.AttrConversationID, convID))
	}
	return tracer.Start(ctx, "memory."+op,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
}

type MemoryDatabase struct {
	db *sql.DB
}

func NewMemoryDatabase(path string) (database.Database, error) {
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

	// Add category column if it doesn't exist (transparent migration)
	if _, err := db.ExecContext(context.Background(), "ALTER TABLE memories ADD COLUMN category TEXT DEFAULT ''"); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("memory database migration failed: %w", err)
		}
	}

	return &MemoryDatabase{db: db}, nil
}

func (m *MemoryDatabase) AddMemory(ctx context.Context, memory database.UserMemory) error {
	ctx, span := startMemorySpan(ctx, "add")
	defer span.End()

	if memory.ID == "" {
		return database.ErrEmptyID
	}
	_, err := m.db.ExecContext(ctx, "INSERT INTO memories (id, created_at, memory, category) VALUES (?, ?, ?, ?)",
		memory.ID, memory.CreatedAt, memory.Memory, memory.Category)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

func (m *MemoryDatabase) GetMemories(ctx context.Context) ([]database.UserMemory, error) {
	ctx, span := startMemorySpan(ctx, "list")
	defer span.End()

	rows, err := m.db.QueryContext(ctx, "SELECT id, created_at, memory, COALESCE(category, '') FROM memories")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var memories []database.UserMemory
	for rows.Next() {
		var memory database.UserMemory
		err := rows.Scan(&memory.ID, &memory.CreatedAt, &memory.Memory, &memory.Category)
		if err != nil {
			return nil, err
		}
		memories = append(memories, memory)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return memories, nil
}

func (m *MemoryDatabase) DeleteMemory(ctx context.Context, memory database.UserMemory) error {
	ctx, span := startMemorySpan(ctx, "delete")
	defer span.End()

	_, err := m.db.ExecContext(ctx, "DELETE FROM memories WHERE id = ?", memory.ID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

func (m *MemoryDatabase) SearchMemories(ctx context.Context, query, category string) (results []database.UserMemory, err error) {
	// SearchMemories is the retrieval shape per the OTel GenAI semconv:
	// the agent is recalling stored memories filtered by query/category.
	// Use the spec'd `retrieval {data_source.id}` span so this lands on
	// the same dashboard row as RAG retrievals.
	ctx, retSpan := genai.StartRetrieval(ctx, "sqlite", memoryDataSourceID, false, "")
	defer func() {
		if err != nil {
			retSpan.RecordError(err, "")
		}
		retSpan.SetResultCount(len(results))
		retSpan.End()
	}()
	if category != "" {
		retSpan.SetAttributes(attribute.String("cagent.memory.category", category))
	}

	// Assign to the named returns (not local shadows) so the deferred
	// span closure observes the live error and result count regardless
	// of which return path fires.
	var conditions []string
	var args []any

	if query != "" {
		words := strings.FieldsSeq(query)
		for word := range words {
			conditions = append(conditions, "LOWER(memory) LIKE LOWER(?) ESCAPE '\\'")
			escaped := strings.ReplaceAll(word, `\`, `\\`)
			escaped = strings.ReplaceAll(escaped, `%`, `\%`)
			escaped = strings.ReplaceAll(escaped, `_`, `\_`)
			args = append(args, "%"+escaped+"%")
		}
	}

	if category != "" {
		conditions = append(conditions, "LOWER(category) = LOWER(?)")
		args = append(args, category)
	}

	stmt := "SELECT id, created_at, memory, COALESCE(category, '') FROM memories"
	if len(conditions) > 0 {
		stmt += " WHERE " + strings.Join(conditions, " AND ") //nolint:gosec // conditions are internal SQL fragments; values are bound parameters
	}

	var rows *sql.Rows
	rows, err = m.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var memory database.UserMemory
		// gocritic suggests `:=` here, but we want to assign to the
		// named return `err` so the deferred span closure observes
		// the failure. nolint pragma documents the intent.
		if err = rows.Scan(&memory.ID, &memory.CreatedAt, &memory.Memory, &memory.Category); err != nil { //nolint:gocritic // assigns to named return `err` for deferred span observability
			return nil, err
		}
		results = append(results, memory)
	}

	if err = rows.Err(); err != nil { //nolint:gocritic // assigns to named return `err` for deferred span observability
		return nil, err
	}

	return results, nil
}

func (m *MemoryDatabase) UpdateMemory(ctx context.Context, memory database.UserMemory) error {
	ctx, span := startMemorySpan(ctx, "update")
	defer span.End()

	if memory.ID == "" {
		return database.ErrEmptyID
	}

	result, err := m.db.ExecContext(ctx, "UPDATE memories SET memory = ?, category = ? WHERE id = ?",
		memory.Memory, memory.Category, memory.ID)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("%w: %s", database.ErrMemoryNotFound, memory.ID)
	}

	return nil
}
