package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestParseCtrlNumberKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		want int
	}{
		{name: "ctrl+1", key: "ctrl+1", want: 0},
		{name: "ctrl+2", key: "ctrl+2", want: 1},
		{name: "ctrl+5", key: "ctrl+5", want: 4},
		{name: "ctrl+9", key: "ctrl+9", want: 8},
		{name: "ctrl+0 (out of range)", key: "ctrl+0", want: -1},
		{name: "no ctrl modifier", key: "1", want: -1},
		{name: "letter key", key: "ctrl+a", want: -1},
		{name: "empty string", key: "", want: -1},
		{name: "ctrl+1+extra (wrong length)", key: "ctrl+1a", want: -1},
		{name: "different prefix", key: "alt+1", want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Construct a KeyPressMsg whose String() reports tt.key. The simplest
			// way is to feed the rune+mod through tea.KeyPressMsg directly when
			// possible; for unsupported strings we synthesize via Code.
			var msg tea.KeyPressMsg
			switch tt.key {
			case "ctrl+0", "ctrl+1", "ctrl+2", "ctrl+3", "ctrl+4", "ctrl+5", "ctrl+6", "ctrl+7", "ctrl+8", "ctrl+9":
				msg = tea.KeyPressMsg{Code: rune(tt.key[5]), Mod: tea.ModCtrl}
			case "alt+1":
				msg = tea.KeyPressMsg{Code: '1', Mod: tea.ModAlt}
			case "ctrl+a":
				msg = tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl}
			case "1":
				msg = tea.KeyPressMsg{Code: '1'}
			case "":
				msg = tea.KeyPressMsg{}
			case "ctrl+1a":
				// Synthesize a key whose String() doesn't match the parser.
				// parseCtrlNumberKey checks len == 6, so a 7-char string
				// reliably falls through to -1.
				msg = tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl | tea.ModAlt}
			}

			got := parseCtrlNumberKey(msg)
			// For the "ctrl+1a" synthetic case the actual String() is whatever
			// bubbletea produces; we only require the function to return -1.
			if tt.key == "ctrl+1a" {
				if got != -1 {
					t.Errorf("parseCtrlNumberKey(%q) = %d, want -1 (any non-numeric ctrl key)", msg.String(), got)
				}
				return
			}
			if got != tt.want {
				t.Errorf("parseCtrlNumberKey(%q) = %d, want %d", msg.String(), got, tt.want)
			}
		})
	}
}

func TestHitTestLeanRegion(t *testing.T) {
	t.Parallel()

	const contentHeight = 20

	tests := []struct {
		name string
		y    int
		want layoutRegion
	}{
		{name: "top of screen", y: 0, want: regionContent},
		{name: "middle of content", y: 10, want: regionContent},
		{name: "last row of content", y: 19, want: regionContent},
		{name: "first row of editor", y: 20, want: regionEditor},
		{name: "deep in editor", y: 100, want: regionEditor},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := hitTestLeanRegion(tt.y, contentHeight); got != tt.want {
				t.Errorf("hitTestLeanRegion(%d, %d) = %v, want %v", tt.y, contentHeight, got, tt.want)
			}
		})
	}
}

func TestHitTestFullRegion(t *testing.T) {
	t.Parallel()

	// Layout (with tab bar present):
	//   y=[0..19]  content       (contentHeight = 20)
	//   y=[20]     resize handle (1 row)
	//   y=[21..22] tab bar       (tabBarHeight = 2)
	//   y=[23..27] editor        (editorHeight = 5)
	//   y>=28      status bar
	const (
		contentHeight = 20
		tabBarHeight  = 2
		editorHeight  = 5
	)

	tests := []struct {
		name string
		y    int
		want layoutRegion
	}{
		{name: "top of content", y: 0, want: regionContent},
		{name: "last row of content", y: 19, want: regionContent},
		{name: "resize handle", y: 20, want: regionResizeHandle},
		{name: "first row of tab bar", y: 21, want: regionTabBar},
		{name: "last row of tab bar", y: 22, want: regionTabBar},
		{name: "first row of editor", y: 23, want: regionEditor},
		{name: "last row of editor", y: 27, want: regionEditor},
		{name: "status bar", y: 28, want: regionStatusBar},
		{name: "far below the screen", y: 100, want: regionStatusBar},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := hitTestFullRegion(tt.y, contentHeight, tabBarHeight, editorHeight)
			if got != tt.want {
				t.Errorf("hitTestFullRegion(%d, %d, %d, %d) = %v, want %v",
					tt.y, contentHeight, tabBarHeight, editorHeight, got, tt.want)
			}
		})
	}
}

func TestHitTestFullRegion_NoTabBar(t *testing.T) {
	t.Parallel()

	// When only one tab is open the tab bar collapses to height 0:
	//   y=[0..19]  content
	//   y=[20]     resize handle
	//   y=[21..25] editor (tabBarHeight = 0, editorHeight = 5)
	//   y>=26      status bar
	const (
		contentHeight = 20
		tabBarHeight  = 0
		editorHeight  = 5
	)

	tests := []struct {
		name string
		y    int
		want layoutRegion
	}{
		{name: "content", y: 10, want: regionContent},
		{name: "resize handle", y: 20, want: regionResizeHandle},
		{name: "editor first row", y: 21, want: regionEditor},
		{name: "editor last row", y: 25, want: regionEditor},
		{name: "status bar", y: 26, want: regionStatusBar},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := hitTestFullRegion(tt.y, contentHeight, tabBarHeight, editorHeight)
			if got != tt.want {
				t.Errorf("hitTestFullRegion(%d, %d, %d, %d) = %v, want %v",
					tt.y, contentHeight, tabBarHeight, editorHeight, got, tt.want)
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
		animFrame    int
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
			animFrame:    0,
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
			animFrame:    1,
			wantContains: []string{" Refactor TUI - docker agent"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := formatWindowTitle(tt.appName, tt.sessionTitle, tt.working, tt.animFrame)
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
