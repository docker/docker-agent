package modelsdev

import (
	"fmt"
	"strings"
)

// ID identifies a model in the models.dev catalog by provider and model
// name. It exists so callers can no longer accidentally pass a bare model
// name (e.g. "claude-sonnet-4-6") where a "provider/model" pair is required:
// the compiler rejects a [string] argument and forces one of the
// constructors below.
//
// The zero value is the empty ID and reports IsZero() == true. Use
// [NewID], [ParseID], or [ParseIDOrZero] to construct values; use
// [ID.String] when a "provider/model" representation is required at a
// boundary (slog fields, event payloads, error messages, ...).
type ID struct {
	Provider string
	Model    string
}

// NewID returns an ID for the given provider and model name. Either
// component may be empty (for example for a provider-less model spec
// during config parsing); call [ID.IsZero] to test for the empty ID and
// [ID.IsValid] to check that both components are populated.
func NewID(provider, model string) ID {
	return ID{Provider: provider, Model: model}
}

// ParseID parses a "provider/model" reference. Either component must be
// non-empty and the separator must be present; otherwise an error is
// returned. The function does not validate that the provider or model
// exists in the models.dev catalog.
func ParseID(ref string) (ID, error) {
	provider, model, ok := strings.Cut(ref, "/")
	if !ok || provider == "" || model == "" {
		return ID{}, fmt.Errorf("invalid model reference %q: expected 'provider/model' format", ref)
	}
	return ID{Provider: provider, Model: model}, nil
}

// ParseIDOrZero parses a "provider/model" reference and returns the zero
// ID when the input is malformed. Use this on best-effort code paths
// (logs, telemetry labels) where a malformed reference should not
// surface as an error.
func ParseIDOrZero(ref string) ID {
	id, err := ParseID(ref)
	if err != nil {
		return ID{}
	}
	return id
}

// String returns the canonical "provider/model" representation. When
// either component is empty the separator is still emitted so the
// output round-trips through [ParseID] only when both fields are set.
// For the zero ID the result is the empty string.
func (id ID) String() string {
	if id.IsZero() {
		return ""
	}
	return id.Provider + "/" + id.Model
}

// IsZero reports whether the ID has both components empty.
func (id ID) IsZero() bool {
	return id.Provider == "" && id.Model == ""
}

// IsValid reports whether both components of the ID are populated.
func (id ID) IsValid() bool {
	return id.Provider != "" && id.Model != ""
}
