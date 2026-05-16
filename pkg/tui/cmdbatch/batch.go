// Package cmdbatch provides a fluent builder for batching tea.Cmd values.
//
// This reduces boilerplate when building command slices in Update functions,
// following the Elm Architecture pattern of accumulating commands.
//
// Usage:
//
//	return m, cmdbatch.New().
//	    Add(p.sidebar.Update(msg)).
//	    AddIf(p.working, p.spinner.Init()).
//	    Add(p.messages.Update(msg)).
//	    Batch()
package cmdbatch

import tea "charm.land/bubbletea/v2"

// Builder accumulates commands for batching.
type Builder struct {
	cmds []tea.Cmd
}

// New creates a new command builder.
func New() *Builder {
	return &Builder{}
}

// Add appends a command to the batch (nil commands are ignored).
func (b *Builder) Add(cmd tea.Cmd) *Builder {
	if cmd != nil {
		b.cmds = append(b.cmds, cmd)
	}
	return b
}

// AddIf conditionally appends a command to the batch.
func (b *Builder) AddIf(condition bool, cmd tea.Cmd) *Builder {
	if condition && cmd != nil {
		b.cmds = append(b.cmds, cmd)
	}
	return b
}

// AddAll appends multiple commands to the batch.
func (b *Builder) AddAll(cmds ...tea.Cmd) *Builder {
	for _, cmd := range cmds {
		if cmd != nil {
			b.cmds = append(b.cmds, cmd)
		}
	}
	return b
}

// Batch returns a batched command, or nil if no commands were added.
func (b *Builder) Batch() tea.Cmd {
	switch len(b.cmds) {
	case 0:
		return nil
	case 1:
		return b.cmds[0]
	default:
		return tea.Batch(b.cmds...)
	}
}

// Len returns the number of commands in the batch.
func (b *Builder) Len() int {
	return len(b.cmds)
}
