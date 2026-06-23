package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/memory/database"
	"github.com/docker/docker-agent/pkg/memory/database/sqlite"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/toolsetpath"
)

const (
	ToolNameAddMemory      = "add_memory"
	ToolNameGetMemories    = "get_memories"
	ToolNameDeleteMemory   = "delete_memory"
	ToolNameSearchMemories = "search_memories"
	ToolNameUpdateMemory   = "update_memory"
)

type DB interface {
	AddMemory(ctx context.Context, memory database.UserMemory) error
	GetMemories(ctx context.Context) ([]database.UserMemory, error)
	DeleteMemory(ctx context.Context, memory database.UserMemory) error
	SearchMemories(ctx context.Context, query, category string) ([]database.UserMemory, error)
	UpdateMemory(ctx context.Context, memory database.UserMemory) error
}

type ToolSet struct {
	db   DB
	path string
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*ToolSet)(nil)
	_ tools.Describer    = (*ToolSet)(nil)
	_ tools.Instructable = (*ToolSet)(nil)
)

// CreateToolSet is used by the tools registry.
func CreateToolSet(toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, configName string) (tools.ToolSet, error) {
	var validatedMemoryPath string

	if toolset.Path != "" {
		var err error
		validatedMemoryPath, err = toolsetpath.Resolve(toolset.Path, parentDir, runConfig)
		if err != nil {
			return nil, fmt.Errorf("invalid memory database path: %w", err)
		}
	} else {
		if configName == "" {
			configName = "default"
		}
		validatedMemoryPath = filepath.Join(paths.GetDataDir(), "memory", sanitizePathSegment(configName), "memory.db")
	}

	if err := os.MkdirAll(filepath.Dir(validatedMemoryPath), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create memory database directory: %w", err)
	}

	db, err := sqlite.NewMemoryDatabase(validatedMemoryPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create memory database: %w", err)
	}

	return NewWithPath(db, validatedMemoryPath), nil
}

// sanitizePathSegment replaces characters that are illegal in a single path
// component on Windows with '_'. Agent sources loaded from an OCI reference
// (e.g. "namespace/repo:tag") produce config names that include the image
// tag's ':'; the colon causes os.MkdirAll to fail with ERROR_INVALID_NAME on
// NTFS. The replacement is lossy but safe — the hash suffix already in the
// config name preserves uniqueness, so collisions from sanitisation aren't a
// concern in practice.
func sanitizePathSegment(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '|', '?', '*', '\\', '/':
			return '_'
		}
		if r < 0x20 {
			return '_'
		}
		return r
	}, s)
}

func New(manager DB) *ToolSet {
	return &ToolSet{
		db: manager,
	}
}

// NewWithPath creates a ToolSet and records the database path for
// user-visible identification in warnings and error messages.
func NewWithPath(manager DB, dbPath string) *ToolSet {
	return &ToolSet{
		db:   manager,
		path: dbPath,
	}
}

// Describe returns a short, user-visible description of this toolset instance.
func (t *ToolSet) Describe() string {
	if t.path != "" {
		return "memory(path=" + t.path + ")"
	}
	return "memory"
}

type AddMemoryArgs struct {
	Memory   string `json:"memory" jsonschema:"The memory content to store"`
	Category string `json:"category,omitempty" jsonschema:"Optional category to organize the memory (e.g. preference, fact, project)"`
}

type DeleteMemoryArgs struct {
	ID string `json:"id" jsonschema:"The ID of the memory to delete"`
}

type SearchMemoriesArgs struct {
	Query    string `json:"query,omitempty" jsonschema:"Keywords to search for in memory content (space-separated, all must match)"`
	Category string `json:"category,omitempty" jsonschema:"Optional category to filter by"`
}

type UpdateMemoryArgs struct {
	ID       string `json:"id" jsonschema:"The ID of the memory to update"`
	Memory   string `json:"memory" jsonschema:"The new memory content"`
	Category string `json:"category,omitempty" jsonschema:"Optional new category for the memory"`
}

func (t *ToolSet) Instructions() string {
	return `## Memory Tools

Check stored memories for relevant context before acting. Store useful information silently — never mention using this tool.

- Remember: user preferences, corrections, key decisions, project conventions
- Use search_memories with keywords/category for targeted lookup; use get_memories only for a full dump
- Use update_memory to edit existing entries; use add_memory only for new information
- Organize with categories: "preference", "fact", "project", "decision"`
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:         ToolNameAddMemory,
			Category:     "memory",
			Description:  "Add a new memory to the database",
			Parameters:   tools.MustSchemaFor[AddMemoryArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleAddMemory),
			Annotations: tools.ToolAnnotations{
				Title: "Add Memory",
			},
		},
		{
			Name:         ToolNameGetMemories,
			Category:     "memory",
			Description:  "Retrieve all stored memories",
			OutputSchema: tools.MustSchemaFor[[]database.UserMemory](),
			Handler:      tools.NewHandler(t.handleGetMemories),
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        "Get Memories",
			},
		},
		{
			Name:         ToolNameDeleteMemory,
			Category:     "memory",
			Description:  "Delete a specific memory by ID",
			Parameters:   tools.MustSchemaFor[DeleteMemoryArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleDeleteMemory),
			Annotations: tools.ToolAnnotations{
				Title: "Delete Memory",
			},
		},
		{
			Name:         ToolNameSearchMemories,
			Category:     "memory",
			Description:  "Search memories by keywords and/or category. More efficient than retrieving all memories.",
			Parameters:   tools.MustSchemaFor[SearchMemoriesArgs](),
			OutputSchema: tools.MustSchemaFor[[]database.UserMemory](),
			Handler:      tools.NewHandler(t.handleSearchMemories),
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        "Search Memories",
			},
		},
		{
			Name:         ToolNameUpdateMemory,
			Category:     "memory",
			Description:  "Update an existing memory's content and/or category by ID",
			Parameters:   tools.MustSchemaFor[UpdateMemoryArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleUpdateMemory),
			Annotations: tools.ToolAnnotations{
				Title: "Update Memory",
			},
		},
	}, nil
}

func (t *ToolSet) handleAddMemory(ctx context.Context, args AddMemoryArgs) (*tools.ToolCallResult, error) {
	memory := database.UserMemory{
		ID:        strconv.FormatInt(time.Now().UnixNano(), 10),
		CreatedAt: time.Now().Format(time.RFC3339),
		Memory:    args.Memory,
		Category:  args.Category,
	}

	if err := t.db.AddMemory(ctx, memory); err != nil {
		return nil, fmt.Errorf("failed to add memory: %w", err)
	}

	return tools.ResultSuccess("Memory added successfully with ID: " + memory.ID), nil
}

func (t *ToolSet) handleGetMemories(ctx context.Context, _ map[string]any) (*tools.ToolCallResult, error) {
	memories, err := t.db.GetMemories(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get memories: %w", err)
	}

	result, err := json.Marshal(memories)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal memories: %w", err)
	}

	return tools.ResultSuccess(string(result)), nil
}

func (t *ToolSet) handleDeleteMemory(ctx context.Context, args DeleteMemoryArgs) (*tools.ToolCallResult, error) {
	memory := database.UserMemory{
		ID: args.ID,
	}

	if err := t.db.DeleteMemory(ctx, memory); err != nil {
		return nil, fmt.Errorf("failed to delete memory: %w", err)
	}

	return tools.ResultSuccess(fmt.Sprintf("Memory with ID %s deleted successfully", args.ID)), nil
}

func (t *ToolSet) handleSearchMemories(ctx context.Context, args SearchMemoriesArgs) (*tools.ToolCallResult, error) {
	memories, err := t.db.SearchMemories(ctx, args.Query, args.Category)
	if err != nil {
		return nil, fmt.Errorf("failed to search memories: %w", err)
	}

	result, err := json.Marshal(memories)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal memories: %w", err)
	}

	return tools.ResultSuccess(string(result)), nil
}

func (t *ToolSet) handleUpdateMemory(ctx context.Context, args UpdateMemoryArgs) (*tools.ToolCallResult, error) {
	memory := database.UserMemory{
		ID:       args.ID,
		Memory:   args.Memory,
		Category: args.Category,
	}

	if err := t.db.UpdateMemory(ctx, memory); err != nil {
		return nil, fmt.Errorf("failed to update memory: %w", err)
	}

	return tools.ResultSuccess(fmt.Sprintf("Memory with ID %s updated successfully", args.ID)), nil
}
