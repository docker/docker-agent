package editorname

import (
	"os"
	"os/exec"
	goruntime "runtime"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFromEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		visual    string
		editorEnv string
		want      string
	}{
		{
			name:      "VSCode",
			visual:    "",
			editorEnv: "code",
			want:      "VSCode",
		},
		{
			name:      "VSCode with args",
			visual:    "",
			editorEnv: "code --wait",
			want:      "VSCode",
		},
		{
			name:      "VSCode with full path",
			visual:    "",
			editorEnv: "/usr/local/bin/code --wait",
			want:      "VSCode",
		},
		{
			name:      "Vim",
			visual:    "",
			editorEnv: "vim",
			want:      "Vim",
		},
		{
			name:      "Neovim",
			visual:    "",
			editorEnv: "nvim",
			want:      "Neovim",
		},
		{
			name:      "Cursor",
			visual:    "",
			editorEnv: "cursor",
			want:      "Cursor",
		},
		{
			name:      "Unknown editor",
			visual:    "",
			editorEnv: "myeditor",
			want:      "Myeditor",
		},
		{
			name:      "Unknown editor with full path",
			visual:    "",
			editorEnv: "/opt/bin/myeditor",
			want:      "Myeditor",
		},
		{
			name:      "Empty (uses platform default)",
			visual:    "",
			editorEnv: "",
			want:      map[bool]string{true: "Notepad", false: "Vi"}[goruntime.GOOS == "windows"],
		},
		{
			name:      "VSCode Insiders",
			visual:    "",
			editorEnv: "code-insiders",
			want:      "VSCode",
		},
		{
			name:      "Neovim Qt",
			visual:    "",
			editorEnv: "nvim-qt",
			want:      "Neovim",
		},
		{
			name:      "Vim GTK",
			visual:    "",
			editorEnv: "vim-gtk3",
			want:      "Vim",
		},
		{
			name:      "VISUAL takes precedence over EDITOR",
			visual:    "code",
			editorEnv: "vim",
			want:      "VSCode",
		},
		{
			name:      "VISUAL with args takes precedence",
			visual:    "code --wait",
			editorEnv: "vim",
			want:      "VSCode",
		},
		{
			name:      "Whitespace-only command falls back to $EDITOR",
			visual:    "",
			editorEnv: "   ",
			want:      "$EDITOR",
		},
		{
			name:      "Unicode whitespace around command",
			editorEnv: "\u2003/usr/bin/vim\u00a0--clean",
			want:      "Vim",
		},
		{
			name:      "Whitespace-only VISUAL masks EDITOR",
			visual:    "\u2003\t\n",
			editorEnv: "vim",
			want:      "$EDITOR",
		},
		{
			name:      "Invalid UTF-8 executable falls back",
			editorEnv: "\xff --wait",
			want:      "$EDITOR",
		},
		{
			name:      "Nano",
			visual:    "",
			editorEnv: "nano",
			want:      "Nano",
		},
		{
			name:      "Emacs",
			visual:    "",
			editorEnv: "emacs -nw",
			want:      "Emacs",
		},
		{
			name:      "Sublime Text via subl",
			visual:    "",
			editorEnv: "subl --wait",
			want:      "Sublime Text",
		},
		{
			name:      "Zed",
			visual:    "",
			editorEnv: "zed --wait",
			want:      "Zed",
		},
		{
			name:      "Unknown editor with multi-byte first rune",
			visual:    "",
			editorEnv: "édit", // U+00E9 (é) is 2 bytes in UTF-8.
			want:      "Édit", // First rune capitalised, rest preserved.
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := FromEnv(tt.visual, tt.editorEnv)
			if got != tt.want {
				t.Errorf("FromEnv(%q, %q) = %v, want %v", tt.visual, tt.editorEnv, got, tt.want)
			}
		})
	}
}

func TestCommandFromEnv(t *testing.T) {
	t.Parallel()

	fallback := "vi"
	if goruntime.GOOS == "windows" {
		fallback = "notepad"
	}

	tests := []struct {
		name      string
		visual    string
		editorEnv string
		wantArgs  []string
	}{
		{
			name:      "EDITOR only",
			editorEnv: "nano",
			wantArgs:  []string{"nano", "/tmp/draft.md"},
		},
		{
			name:      "VISUAL takes precedence over EDITOR",
			visual:    "code --wait",
			editorEnv: "vim",
			wantArgs:  []string{"code", "--wait", "/tmp/draft.md"},
		},
		{
			name:      "extra tokens stay ordered before the path",
			editorEnv: "emacs -nw -q",
			wantArgs:  []string{"emacs", "-nw", "-q", "/tmp/draft.md"},
		},
		{
			name:     "neither set falls back to the platform editor",
			wantArgs: []string{fallback, "/tmp/draft.md"},
		},
		{
			// cmp.Or picks the non-empty VISUAL even when it is only
			// whitespace, so EDITOR is masked and the fallback launches.
			name:      "whitespace-only VISUAL masks EDITOR",
			visual:    "   ",
			editorEnv: "vim",
			wantArgs:  []string{fallback, "/tmp/draft.md"},
		},
		{
			// strings.Fields, not a shell: quotes are ordinary characters.
			name:      "no shell evaluation",
			editorEnv: `vim -c "set ft"`,
			wantArgs:  []string{"vim", "-c", `"set`, `ft"`, "/tmp/draft.md"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := CommandFromEnv(tt.visual, tt.editorEnv, "/tmp/draft.md")
			assert.Equal(t, tt.wantArgs, cmd.Args)
			assertRealTerminalStdio(t, cmd)
		})
	}
}

func TestCommandReadsEnvironment(t *testing.T) {
	t.Setenv("VISUAL", "code --wait")
	t.Setenv("EDITOR", "vim")

	cmd := Command("/tmp/draft.md")
	assert.Equal(t, []string{"code", "--wait", "/tmp/draft.md"}, cmd.Args)
	assertRealTerminalStdio(t, cmd)
}

// assertRealTerminalStdio checks that the command's stdio is exactly the
// process's OS files. Nil entries would let tea.ExecProcess substitute the
// Program's image-writer output, which is not an *os.File — the editor then
// runs against a pipe (#3873, "Vim: Warning: Output is not to a terminal").
func assertRealTerminalStdio(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	assert.Same(t, os.Stdin, cmd.Stdin, "Stdin must be os.Stdin")
	assert.Same(t, os.Stdout, cmd.Stdout, "Stdout must be os.Stdout")
	assert.Same(t, os.Stderr, cmd.Stderr, "Stderr must be os.Stderr")
}
