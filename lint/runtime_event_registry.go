package main

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"

	"github.com/dgageot/rubocop-go/cop"
)

// RuntimeEventRegistry enforces that pkg/runtime/client.go registers every
// runtime event type defined in pkg/runtime/event.go.
//
// The remote-runtime client decodes incoming JSON events by reading the
// "type" discriminator and looking it up in a static registry built in
// pkg/runtime/client.go:
//
//	registry: map[string]func() Event{
//	    "user_message": func() Event { return &UserMessageEvent{} },
//	    …
//	}
//
// When an event type appears in the JSON stream but not in the registry the
// client logs a single Debug-level "invalid_type" line and silently drops
// the event. That is essentially invisible: a sub-agent's
// MessageAddedEvent, ModelFallbackEvent or SubSessionCompletedEvent
// reaching a remote frontend would never be rendered, with no visible
// error.
//
// The cop runs on pkg/runtime/client.go (the registry's home) and
// cross-references every &XxxEvent{Type: "yyy"} composite literal it
// finds in pkg/runtime/event.go. Each event type whose Type-string is
// missing from the registry is reported with a diagnostic anchored on
// the registry map literal, where the fix lives.
var RuntimeEventRegistry = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/RuntimeEventRegistry",
		Description: "pkg/runtime/client.go must register every runtime event type",
		Severity:    cop.Error,
	},
	Scope: cop.OnlyFile("pkg/runtime/client.go"),
	Run: func(p *cop.Pass) {
		// Sibling event.go is the source of truth for the set of emitted events.
		eventFile, err := p.ParseSibling("event.go")
		if err != nil {
			return
		}
		emitted := emittedEventTypes(eventFile)
		if len(emitted) == 0 {
			return
		}

		registryNode, registered := registryEntries(p)
		if registryNode == nil {
			return
		}

		var missing []string
		for typeName, typeStr := range emitted {
			if got, ok := registered[typeStr]; !ok {
				missing = append(missing, typeName+" (Type: "+strconv.Quote(typeStr)+")")
			} else if got != typeName {
				// The registry key is correct but it constructs the wrong
				// concrete type. That is a different bug from "missing entry"
				// so we keep it as a separate diagnostic anchored on the
				// registry map for clarity.
				p.Reportf(registryNode,
					"registry key %q constructs %s but %s emits Type=%q",
					typeStr, got, typeName, typeStr)
			}
		}
		p.ReportMissing(registryNode,
			"pkg/runtime/client.go is missing registry entries for: %s", missing)
	},
}

// emittedEventTypes returns a map of EventTypeName -> wire-format Type
// string for every &XxxEvent{Type: "yyy"} composite literal in the given
// file (typically pkg/runtime/event.go). Constructors that don't initialise
// the Type field are skipped.
func emittedEventTypes(file *ast.File) map[string]string {
	emitted := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		id, ok := cl.Type.(*ast.Ident)
		if !ok || !strings.HasSuffix(id.Name, "Event") {
			return true
		}
		if val, ok := cop.StringField(cl, "Type"); ok {
			emitted[id.Name] = val
		}
		return true
	})
	return emitted
}

// registryEntries finds the registry map literal in pkg/runtime/client.go and
// returns it together with a typeString -> EventTypeName mapping built from
// its `"type-string": func() Event { return &XxxEvent{} }` entries.
//
// The registry is identified syntactically as the only map[string]func() Event
// composite literal in the file. Returning the literal itself lets the
// caller anchor diagnostics on the map so the diff is obvious.
func registryEntries(p *cop.Pass) (ast.Node, map[string]string) {
	var found *ast.CompositeLit
	registered := map[string]string{}
	ast.Inspect(p.File, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok || !isEventRegistryType(cl.Type) {
			return true
		}
		found = cl
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			klit, ok := kv.Key.(*ast.BasicLit)
			if !ok || klit.Kind != token.STRING {
				continue
			}
			key, err := strconv.Unquote(klit.Value)
			if err != nil {
				continue
			}
			if name, ok := returnedEventType(kv.Value); ok {
				registered[key] = name
			}
		}
		return false
	})
	if found == nil {
		return nil, nil
	}
	return found, registered
}

// isEventRegistryType reports whether expr describes the registry's value
// type, i.e. `map[string]func() Event`. The check is syntactic: the cop
// runs without type information.
func isEventRegistryType(expr ast.Expr) bool {
	mt, ok := expr.(*ast.MapType)
	if !ok {
		return false
	}
	if k, ok := mt.Key.(*ast.Ident); !ok || k.Name != "string" {
		return false
	}
	ft, ok := mt.Value.(*ast.FuncType)
	if !ok {
		return false
	}
	return cop.IsNullarySig(ft, "Event")
}

// returnedEventType pulls "FooEvent" out of a `func() Event { return &FooEvent{} }`
// literal. Returns "" if the closure has any other shape, in which case the
// cop simply ignores the entry (an unrelated map happening to share the
// registry's value type would otherwise produce noise).
func returnedEventType(expr ast.Expr) (string, bool) {
	fl, ok := expr.(*ast.FuncLit)
	if !ok || fl.Body == nil {
		return "", false
	}
	for _, stmt := range fl.Body.List {
		ret, ok := stmt.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			continue
		}
		ue, ok := ret.Results[0].(*ast.UnaryExpr)
		if !ok {
			continue
		}
		cl, ok := ue.X.(*ast.CompositeLit)
		if !ok {
			continue
		}
		if id, ok := cl.Type.(*ast.Ident); ok {
			return id.Name, true
		}
	}
	return "", false
}
