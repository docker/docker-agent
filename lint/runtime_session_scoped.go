package main

import (
	"go/ast"
	"strings"

	"github.com/dgageot/rubocop-go/cop"
)

// RuntimeSessionScoped enforces that every runtime event struct carrying a
// SessionID field also implements the SessionScoped interface, i.e. exposes
// a `GetSessionID() string` method on its pointer receiver.
//
// SessionScoped is the discriminator the persistence pipeline uses to keep
// sub-agent events out of the parent session's transcript:
//
//	if scoped, ok := event.(SessionScoped); ok && scoped.GetSessionID() != sess.ID {
//	    return // forwarded sub-agent event for a different session — drop
//	}
//
// An event that carries SessionID but skips the method silently bypasses the
// filter — the type assertion fails, the conditional short-circuits, and
// the event is persisted into whatever session happens to be on the parent
// observer when it arrives. That contaminates transcripts with sub-agent
// chatter and there is no signal at the call site to suggest the rule was
// missed.
//
// The cop runs on pkg/runtime/event.go and reports any *Event struct with
// a SessionID field whose pointer type lacks GetSessionID().
var RuntimeSessionScoped = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/RuntimeSessionScoped",
		Description: "runtime events with a SessionID field must implement GetSessionID() (SessionScoped)",
		Severity:    cop.Error,
	},
	Scope: cop.OnlyFile("pkg/runtime/event.go"),
	Run: func(p *cop.Pass) {
		withSessionID := eventStructsWithSessionID(p)
		if len(withSessionID) == 0 {
			return
		}
		implementsScoped := p.PointerReceiverMethods("GetSessionID")

		for typeName, typeSpec := range withSessionID {
			if !implementsScoped[typeName] {
				p.Reportf(typeSpec.Name,
					"%s carries a SessionID field but does not implement GetSessionID() (SessionScoped); "+
						"sub-agent events of this type would bypass the persistence-observer session filter",
					typeName)
			}
		}
	},
}

// eventStructsWithSessionID maps EventTypeName -> its declaring *ast.TypeSpec
// for every top-level struct type whose name ends in "Event" and that
// declares a SessionID field. Embedded fields are out of scope.
func eventStructsWithSessionID(p *cop.Pass) map[string]*ast.TypeSpec {
	out := map[string]*ast.TypeSpec{}
	p.ForEachStruct(func(ts *ast.TypeSpec, st *ast.StructType) {
		if !strings.HasSuffix(ts.Name.Name, "Event") || st.Fields == nil {
			return
		}
		for _, fld := range st.Fields.List {
			for _, name := range fld.Names {
				if name.Name == "SessionID" {
					out[ts.Name.Name] = ts
				}
			}
		}
	})
	return out
}
