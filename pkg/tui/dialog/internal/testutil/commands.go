// Package testutil provides command inspection for dialog tests.
package testutil

import (
	"reflect"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// CollectMsgs executes a command (or batch/sequence of commands) and collects all returned messages.
// It handles tea.BatchMsg and tea.Sequence (which uses an unexported slice type).
func CollectMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}

	msg := cmd()
	if msg == nil {
		return nil
	}

	// Handle BatchMsg
	if batchMsg, ok := msg.(tea.BatchMsg); ok {
		var msgs []tea.Msg
		for _, innerCmd := range batchMsg {
			if innerCmd != nil {
				msgs = append(msgs, CollectMsgs(innerCmd)...)
			}
		}
		return msgs
	}

	// Handle Sequence (unexported type, use reflection)
	// tea.Sequence returns a func that returns a sequenceMsg which is []tea.Cmd
	msgValue := reflect.ValueOf(msg)
	if msgValue.Kind() == reflect.Slice {
		var msgs []tea.Msg
		for i := range msgValue.Len() {
			elem := msgValue.Index(i)
			if elem.CanInterface() {
				if innerCmd, ok := reflect.TypeAssert[tea.Cmd](elem); ok && innerCmd != nil {
					msgs = append(msgs, CollectMsgs(innerCmd)...)
				}
			}
		}
		if len(msgs) > 0 {
			return msgs
		}
	}

	return []tea.Msg{msg}
}

// FindMsg searches for a message of the specified type in the collected messages.
func FindMsg[T any](msgs []tea.Msg) (T, bool) {
	var zero T
	for _, msg := range msgs {
		if typed, ok := msg.(T); ok {
			return typed, true
		}
	}
	return zero, false
}

// HasMsg checks if a message of the specified type exists in the collected messages.
func HasMsg[T any](msgs []tea.Msg) bool {
	_, found := FindMsg[T](msgs)
	return found
}

// SequenceCommands exposes ordered steps without running them, rejecting batches.
func SequenceCommands(tb testing.TB, cmd tea.Cmd) []tea.Cmd {
	tb.Helper()
	if cmd == nil {
		tb.Fatal("expected a sequence, got nil")
	}
	msg := cmd()
	value := reflect.ValueOf(msg)
	if !value.IsValid() || value.Kind() != reflect.Slice || value.Type().Name() != "sequenceMsg" {
		tb.Fatalf("expected tea.Sequence, got %T", msg)
	}
	cmds := make([]tea.Cmd, value.Len())
	for i := range cmds {
		var ok bool
		cmds[i], ok = reflect.TypeAssert[tea.Cmd](value.Index(i))
		if !ok || cmds[i] == nil {
			tb.Fatalf("invalid sequence command at index %d", i)
		}
	}
	return cmds
}
