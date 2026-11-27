package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/commands"
)

func TestParseCtrlNumberKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  tea.KeyPressMsg
		want int
	}{
		{name: "ctrl+1", msg: tea.KeyPressMsg{Code: '1', Mod: tea.ModCtrl}, want: 0},
		{name: "ctrl+2", msg: tea.KeyPressMsg{Code: '2', Mod: tea.ModCtrl}, want: 1},
		{name: "ctrl+5", msg: tea.KeyPressMsg{Code: '5', Mod: tea.ModCtrl}, want: 4},
		{name: "ctrl+9", msg: tea.KeyPressMsg{Code: '9', Mod: tea.ModCtrl}, want: 8},
		{name: "ctrl+0 (out of range)", msg: tea.KeyPressMsg{Code: '0', Mod: tea.ModCtrl}, want: -1},
		{name: "no ctrl modifier", msg: tea.KeyPressMsg{Code: '1'}, want: -1},
		{name: "letter key", msg: tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl}, want: -1},
		{name: "empty key", msg: tea.KeyPressMsg{}, want: -1},
		{name: "ctrl+alt+a", msg: tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl | tea.ModAlt}, want: -1},
		{name: "alt+1", msg: tea.KeyPressMsg{Code: '1', Mod: tea.ModAlt}, want: -1},
		// Kitty keyboard protocol populates Text, which makes String() report
		// the bare digit instead of "ctrl+1". The parser must still match.
		{name: "ctrl+1 with kitty text", msg: tea.KeyPressMsg{Code: '1', Text: "1", Mod: tea.ModCtrl}, want: 0},
		// International layout: BaseCode carries the PC-101 digit.
		{name: "ctrl+3 via BaseCode", msg: tea.KeyPressMsg{Code: '"', BaseCode: '3', Mod: tea.ModCtrl}, want: 2},
		// ctrl+alt+1 must not match (extra modifier).
		{name: "ctrl+alt+1", msg: tea.KeyPressMsg{Code: '1', Mod: tea.ModCtrl | tea.ModAlt}, want: -1},
		// Lock states (Caps/Num Lock) ride along under the Kitty protocol but
		// must be ignored so the shortcut still fires.
		{name: "ctrl+1 with caps lock", msg: tea.KeyPressMsg{Code: '1', Mod: tea.ModCtrl | tea.ModCapsLock}, want: 0},
		{name: "ctrl+4 with num lock", msg: tea.KeyPressMsg{Code: '4', Mod: tea.ModCtrl | tea.ModNumLock}, want: 3},
		// Lock state alone (no Ctrl) must not match.
		{name: "caps lock only", msg: tea.KeyPressMsg{Code: '1', Mod: tea.ModCapsLock}, want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseCtrlNumberKey(tt.msg); got != tt.want {
				t.Errorf("parseCtrlNumberKey(%+v) = %d, want %d", tt.msg, got, tt.want)
			}
		})
	}
}

func TestFormatWindowTitle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		appName      string
		sessionTitle string
		working      bool
		wantContains []string
		wantEquals   string
	}{
		{
			name:         "idle, no session title",
			appName:      "docker agent",
			sessionTitle: "",
			working:      false,
			wantEquals:   "docker agent",
		},
		{
			name:         "idle with session title",
			appName:      "docker agent",
			sessionTitle: "Refactor TUI",
			working:      false,
			wantEquals:   "Refactor TUI - docker agent",
		},
		{
			name:         "working prepends a spinner frame",
			appName:      "docker agent",
			sessionTitle: "",
			working:      true,
			// Spinner frame is a single rune followed by a space, then the
			// app name. We don't pin the exact rune (it depends on the
			// spinner package) but we do guarantee the suffix.
			wantContains: []string{" docker agent"},
		},
		{
			name:         "working with session title",
			appName:      "docker agent",
			sessionTitle: "Refactor TUI",
			working:      true,
			wantContains: []string{" Refactor TUI - docker agent"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := formatWindowTitle(0, tt.appName, tt.sessionTitle, tt.working)
			if tt.wantEquals != "" && got != tt.wantEquals {
				t.Errorf("formatWindowTitle = %q, want %q", got, tt.wantEquals)
			}
			for _, sub := range tt.wantContains {
				if !strings.Contains(got, sub) {
					t.Errorf("formatWindowTitle = %q, want to contain %q", got, sub)
				}
			}
		})
	}
}

func TestCommandCategories_DisabledCommandsFilter(t *testing.T) {
	t.Parallel()

	build := func(context.Context, tea.Model) []commands.Category {
		return []commands.Category{
			{
				Name: "Session",
				Commands: []commands.Item{
					{ID: "a", SlashCommand: "/cost"},
					{ID: "b", SlashCommand: "/eval"},
					{ID: "c", SlashCommand: "/exit"},
				},
			},
			{
				Name: "Settings",
				Commands: []commands.Item{
					{ID: "d", SlashCommand: "/theme"},
				},
			},
		}
	}

	t.Run("no filter keeps everything", func(t *testing.T) {
		t.Parallel()
		m := &appModel{ctx: t.Context, buildCommandCategories: build}
		got := m.commandCategories()
		if len(got) != 2 {
			t.Fatalf("len(categories) = %d, want 2", len(got))
		}
	})

	t.Run("filters slash commands and drops empty categories", func(t *testing.T) {
		t.Parallel()
		m := &appModel{ctx: t.Context, buildCommandCategories: build}
		WithDisabledCommands([]string{"/cost", "eval", "/theme"})(m)

		got := m.commandCategories()
		if len(got) != 1 {
			t.Fatalf("len(categories) = %d, want 1 (Settings dropped, Session kept)", len(got))
		}
		if got[0].Name != "Session" {
			t.Fatalf("category = %q, want Session", got[0].Name)
		}
		if len(got[0].Commands) != 1 || got[0].Commands[0].SlashCommand != "/exit" {
			t.Fatalf("session commands = %+v, want only /exit", got[0].Commands)
		}
	})

	t.Run("blank entries are ignored", func(t *testing.T) {
		t.Parallel()
		m := &appModel{ctx: t.Context, buildCommandCategories: build}
		WithDisabledCommands([]string{"", "  "})(m)
		got := m.commandCategories()
		if len(got) != 2 {
			t.Fatalf("len(categories) = %d, want 2", len(got))
		}
	})

	t.Run("matching is case-insensitive", func(t *testing.T) {
		t.Parallel()
		m := &appModel{ctx: t.Context, buildCommandCategories: build}
		WithDisabledCommands([]string{"/Cost", "EVAL", "/Theme"})(m)
		got := m.commandCategories()
		if len(got) != 1 {
			t.Fatalf("len(categories) = %d, want 1 (Settings dropped, Session kept)", len(got))
		}
		if got[0].Name != "Session" {
			t.Fatalf("category = %q, want Session", got[0].Name)
		}
		if len(got[0].Commands) != 1 || got[0].Commands[0].SlashCommand != "/exit" {
			t.Fatalf("session commands = %+v, want only /exit", got[0].Commands)
		}
	})
}
