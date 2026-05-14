// Package harness defines the cross-harness orchestration layer for docker-agent.
// It re-exports the [github.com/rumpl/harness] Provider/Event types as the
// public API, and adds docker-agent-specific request/result types and a
// process-local adapter registry.
//
// # Alignment with rumpl/harness
//
// The Provider interface, Event type, EventType constants, and Usage struct
// come from rumpl/harness via type aliases so that providers implemented
// against either package are interchangeable. docker-agent owns the
// SubSessionRequest, RunResult, ACPCallbacks, and ErrorCode vocabulary that
// rumpl/harness does not model.
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	extharness "github.com/rumpl/harness"

	"github.com/docker/docker-agent/pkg/chat"
)

// Provider is the harness provider interface. Aliased from rumpl/harness so
// providers built against either package are compatible.
type Provider = extharness.Provider

// Event is a single parsed event from a provider's streaming output.
type Event = extharness.Event

// EventType enumerates the kinds of events a Provider can produce.
type EventType = extharness.EventType

// Usage captures token and cost statistics from a completed run.
type Usage = extharness.Usage

// Event type constants re-exported from rumpl/harness.
const (
	EventText     = extharness.EventText
	EventResult   = extharness.EventResult
	EventToolCall = extharness.EventToolCall
)

// ErrorCode classifies terminal harness errors for the orchestrator.
type ErrorCode string

const (
	ErrCodeContextExhausted   ErrorCode = "context_exhausted"
	ErrCodeRateLimited        ErrorCode = "rate_limited"
	ErrCodeAuthFailed         ErrorCode = "auth_failed"
	ErrCodeHarnessCrashed     ErrorCode = "harness_crashed"
	ErrCodeHarnessTimeout     ErrorCode = "harness_timeout"
	ErrCodeUserCanceled       ErrorCode = "user_canceled"
	ErrCodeCapabilityMismatch ErrorCode = "capability_mismatch"
	ErrCodeUnknown            ErrorCode = "unknown"
)

// SubSessionRequest is the docker-agent input to a harness sub-session run.
type SubSessionRequest struct {
	RunID, ParentID string

	// SystemPrompt is the agent's instruction. Some providers (e.g. OpenCode
	// CLI) do not support per-call system prompts; they prepend it to Task.
	SystemPrompt string

	// Task is the user message / task description for this sub-session.
	Task string

	// ResumeToken is a provider-opaque token from a prior RunResult.HarnessRunID.
	// Non-empty means resume mode: the provider uses native session resume and
	// ignores SimulatedHistory.
	ResumeToken string

	// SimulatedHistory is prior conversation turns to prepend to the system
	// prompt. Only used when ResumeToken == "".
	SimulatedHistory []chat.Message

	WorkingDir string
	Env        map[string]string

	// Config is the provider-specific config marshaled to JSON for the
	// provider to unmarshal into its own typed struct.
	Config json.RawMessage

	Timeout time.Duration
}

// RunResult is the terminal result of a harness sub-session.
type RunResult struct {
	// FinalText is the assistant's final answer text.
	FinalText string

	// Usage carries token and cost information when reported by the provider.
	Usage *Usage

	// HarnessRunID is the provider-opaque token for session resumption.
	// Store via session.SetHarnessToken(agentName, HarnessRunID).
	HarnessRunID string

	// Err is the terminal error, if any.
	Err error

	// ErrCode classifies Err for the orchestrator. Empty when Err == nil.
	ErrCode ErrorCode
}

// ToolExecutor executes host-side tools on behalf of ACP providers. The
// method name matches the ACP wire method (e.g. "fs/read_text_file").
type ToolExecutor interface {
	Execute(ctx context.Context, method string, params []byte) ([]byte, error)
}

// PermissionRequester handles synchronous permission decisions for ACP
// providers. Returns allowed=true if the decision permits the tool call,
// plus the source of the decision ("user", "policy", "remembered", "timeout").
type PermissionRequester interface {
	Request(ctx context.Context, toolCallID, toolName, description string, options []string) (allowed bool, source string, err error)
}

// ACPCallbacks provides host-side services required by ACP providers.
type ACPCallbacks struct {
	ToolExecutor ToolExecutor
	Permission   PermissionRequester
}

// Registry of process-local providers keyed by Name().
var (
	regMu    sync.RWMutex
	registry = map[string]Provider{}

	tokenMu    sync.Mutex
	tokenInUse = map[string]bool{}
)

// Register registers a provider by name. Typically called from provider
// init() functions. Panics if a provider with the same name is already
// registered.
func Register(p Provider) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, exists := registry[p.Name()]; exists {
		panic(fmt.Sprintf("harness: provider %q already registered", p.Name()))
	}
	registry[p.Name()] = p
}

// Lookup returns the provider registered for the given name.
func Lookup(name string) (Provider, error) {
	regMu.RLock()
	defer regMu.RUnlock()
	p, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("harness: no provider registered for type %q", name)
	}
	return p, nil
}

// AcquireToken marks a resume token as in-use for the duration of a
// sub-session. Returns an error if the token is already acquired by another
// active sub-session. Call ReleaseToken when the sub-session ends.
func AcquireToken(token string) error {
	if token == "" {
		return nil
	}
	tokenMu.Lock()
	defer tokenMu.Unlock()
	if tokenInUse[token] {
		return fmt.Errorf("harness: session token %q is already in use by another active sub-session; concurrent reuse is not supported", token)
	}
	tokenInUse[token] = true
	return nil
}

// ReleaseToken marks a resume token as no longer in use.
func ReleaseToken(token string) {
	if token == "" {
		return
	}
	tokenMu.Lock()
	defer tokenMu.Unlock()
	delete(tokenInUse, token)
}
