package types

// Document represents a document for reranking with content and metadata
// This is a generic structure that can be used across the RAG system
type Document struct {
	Content    string            // The document text/chunk content
	SourcePath string            // File path or document identifier
	Metadata   map[string]string // Custom metadata (e.g., date, author, type, tags)
	ChunkIndex int               // Position of this chunk within the source document (0-based)
}

type EventTye string

const (
	EventTypeIndexingStarted  EventTye = "indexing_started"
	EventTypeIndexingProgress EventTye = "indexing_progress"
	EventTypeIndexingComplete EventTye = "indexing_complete"
	EventTypeUsage            EventTye = "usage"
	EventTypeError            EventTye = "error"
)

// Event represents a RAG operation lifecycle event.
// This is the canonical RAG event type used by strategies, reranking, fusion,
// the RAG manager, and the runtime.
type Event struct {
	Type         EventTye
	StrategyName string // Name of the component emitting the event (strategy name, "reranker", "fusion", etc.)
	Message      string
	Progress     *Progress
	Error        error
	TotalTokens  int64   // For usage events
	Cost         float64 // For usage events
}

// Progress represents progress within a multi-step operation (e.g., indexing, reranking).
type Progress struct {
	Current int
	Total   int
}

// EventCallback receives RAG manager events during initialization.
type EventCallback func(event Event)

// EventForwarder is implemented by RAG toolsets that can stream lifecycle
// events (indexing progress, usage, errors) to a callback. The runtime
// type-asserts to this interface to wire event forwarding without importing
// the concrete rag toolset package — which keeps the cgo tree-sitter
// dependency out of the runtime's import graph.
type EventForwarder interface {
	// Name returns the toolset's user-facing name.
	Name() string
	// SetEventCallback registers the callback; must be called before Start.
	SetEventCallback(callback EventCallback)
}
