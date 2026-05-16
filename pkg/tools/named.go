package tools

// Named is implemented by toolsets that carry a user-visible name.
//
// The convention is:
//   - For toolsets that take a `name:` in their YAML entry (currently MCP,
//     A2A, OpenAPI), Name() returns that user-set value.
//   - For built-in toolsets that don't expose a `name:` field
//     (`shell`, `filesystem`, `memory`, …), the registry wraps the
//     toolset with WithName(toolset.Type) so Name() returns the YAML
//     `type:` instead. The point is that no surface ever has to display
//     a Go type ("*builtin.Tool") to the user.
type Named interface {
	Name() string
}

// GetName returns the toolset's Name when an inner toolset advertises one
// via Named, walking through any Unwrapper-reachable wrapper chain. The
// empty string is returned when no wrapper provides a name.
func GetName(ts ToolSet) string {
	if n, ok := As[Named](ts); ok {
		return n.Name()
	}
	return ""
}

// WithName returns ts wrapped so it advertises Name() == name. If ts (or
// any inner toolset reachable via Unwrap) already advertises a non-empty
// Name(), ts is returned unchanged so the original implementation wins.
//
// The returned wrapper participates in As[T]: every capability of ts
// remains reachable through the wrapper.
func WithName(ts ToolSet, name string) ToolSet {
	if ts == nil || name == "" {
		return ts
	}
	if existing, ok := As[Named](ts); ok && existing.Name() != "" {
		return ts
	}
	return &namedToolSet{ToolSet: ts, name: name}
}

// namedToolSet decorates a ToolSet with a stable Name(). Every other
// capability is delegated to the inner toolset via the embedded ToolSet
// interface; As[T] reaches the inner type through Unwrap so e.g.
// Statable, Restartable and Kinder remain visible through the wrapper.
type namedToolSet struct {
	ToolSet

	name string
}

// Compile-time guarantee that namedToolSet exposes Named and Unwrapper.
var (
	_ Named     = (*namedToolSet)(nil)
	_ Unwrapper = (*namedToolSet)(nil)
)

func (n *namedToolSet) Name() string    { return n.name }
func (n *namedToolSet) Unwrap() ToolSet { return n.ToolSet }
