package tools

import "reflect"

// Composite is implemented by toolsets that expose multiple child toolsets.
// Children are discoverable through the graph but remain owned by the
// composite; traversal does not imply lifecycle ownership.
type Composite interface {
	Children() []ToolSet
}

// Walk visits a toolset graph depth-first in deterministic, outer-first order.
// Decorated children are visited before composite children, which retain their
// declared order. Returning false skips the current node's descendants. Shared
// comparable nodes are visited once, which also breaks cycles among the pointer
// implementations used by production toolsets.
func Walk(root ToolSet, visit func(ToolSet) bool) {
	seen := make(map[ToolSet]struct{})

	var walk func(ToolSet)
	walk = func(ts ToolSet) {
		if ts == nil || seenToolSet(ts, seen) {
			return
		}
		if !visit(ts) {
			return
		}
		if unwrapper, ok := ts.(Unwrapper); ok {
			walk(unwrapper.Unwrap())
		}
		if composite, ok := ts.(Composite); ok {
			for _, child := range composite.Children() {
				walk(child)
			}
		}
	}

	walk(root)
}

func seenToolSet(ts ToolSet, seen map[ToolSet]struct{}) bool {
	value := reflect.ValueOf(ts)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return true
		}
	}
	if !value.Comparable() {
		return false
	}
	if _, exists := seen[ts]; exists {
		return true
	}
	seen[ts] = struct{}{}
	return false
}

// Find returns the first matching capability in graph traversal order.
func Find[T any](root ToolSet) (T, bool) {
	var result T
	found := false
	Walk(root, func(ts ToolSet) bool {
		if found {
			return false
		}
		if capability, ok := ts.(T); ok {
			result = capability
			found = true
			return false
		}
		return true
	})
	return result, found
}

// FindAll returns every matching capability in graph traversal order.
func FindAll[T any](root ToolSet) []T {
	var result []T
	Walk(root, func(ts ToolSet) bool {
		if capability, ok := ts.(T); ok {
			result = append(result, capability)
		}
		return true
	})
	return result
}
