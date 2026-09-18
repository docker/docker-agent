package main

import (
	"testing"

	"github.com/dgageot/rubocop-go/coptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTUIKeyBindingsFlagsNamedKeyStringCases(t *testing.T) {
	t.Parallel()
	src := `package p
func handle(msg interface{ String() string }) {
	switch msg.String() {
	case "esc", "q":
	case "up", "k", "ctrl+k":
	case "enter":
	}
}
`
	offenses := coptest.RunNamed(t, TUIKeyBindings, "pkg/tui/dialog/picker.go", src)
	require.Len(t, offenses, 4)
	assert.Contains(t, offenses[0].Message, `named key "esc"`)
	assert.Contains(t, offenses[1].Message, `named key "up"`)
	assert.Contains(t, offenses[2].Message, `named key "ctrl+k"`)
	assert.Contains(t, offenses[3].Message, `named key "enter"`)
}

func TestTUIKeyBindingsFlagsModifiedNamedKeyStringCases(t *testing.T) {
	t.Parallel()
	src := `package p
func handle(msg interface{ String() string }) {
	switch msg.String() {
	case "ctrl+k", "shift+tab":
	}
}
`
	offenses := coptest.RunNamed(t, TUIKeyBindings, "pkg/tui/dialog/picker.go", src)
	require.Len(t, offenses, 2)
	assert.Contains(t, offenses[0].Message, `named key "ctrl+k"`)
	assert.Contains(t, offenses[1].Message, `named key "shift+tab"`)
}

func TestTUIKeyBindingsAllowsRuneKeyStringCases(t *testing.T) {
	t.Parallel()
	src := `package p
func handle(msg interface{ String() string }) {
	switch msg.String() {
	case "q", "k":
	}
}
`
	assert.Empty(t, coptest.RunNamed(t, TUIKeyBindings, "pkg/tui/dialog/picker.go", src))
}

func TestTUIKeyBindingsFlagsNamedKeyComparisons(t *testing.T) {
	t.Parallel()
	src := `package p
func handle(msg interface{ String() string }) {
	if msg.String() == "space" || "ctrl+g" == msg.String() {}
	if msg.String() != "enter" {}
}
`
	offenses := coptest.RunNamed(t, TUIKeyBindings, "pkg/tui/dialog/picker.go", src)
	require.Len(t, offenses, 3)
	assert.Contains(t, offenses[0].Message, `named key "space"`)
	assert.Contains(t, offenses[1].Message, `named key "ctrl+g"`)
	assert.Contains(t, offenses[2].Message, `named key "enter"`)
}

func TestTUIKeyBindingsAllowsRuneKeyComparisons(t *testing.T) {
	t.Parallel()
	src := `package p
func handle(msg interface{ String() string }) {
	if msg.String() == "q" {}
}
`
	assert.Empty(t, coptest.RunNamed(t, TUIKeyBindings, "pkg/tui/dialog/picker.go", src))
}

func TestTUIKeyBindingsAllowsNonStringSwitches(t *testing.T) {
	t.Parallel()
	src := `package p
func handle(code string) {
	switch code {
	case "esc":
	}
}
`
	assert.Empty(t, coptest.RunNamed(t, TUIKeyBindings, "pkg/tui/dialog/picker.go", src))
}

func TestTUIKeyBindingsIgnoresFilesOutsideTUI(t *testing.T) {
	t.Parallel()
	src := `package p
func handle(msg interface{ String() string }) {
	switch msg.String() {
	case "esc":
	}
}
`
	assert.Empty(t, coptest.RunNamed(t, TUIKeyBindings, "pkg/server/server.go", src))
}
