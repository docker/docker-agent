package builtin

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		content      string
		find         string
		wantText     string // expected SearchText (empty → use find)
		wantStrategy string
		wantErr      string // substring of error; empty → expect success
	}{
		// Exact
		{
			name:         "exact",
			content:      "hello world\nfoo bar\nbaz",
			find:         "foo bar",
			wantStrategy: "exact",
		},
		{
			name:         "exact when both sides escaped",
			content:      `echo \"hello\"`,
			find:         `echo \"hello\"`,
			wantStrategy: "exact",
		},

		// Escape-normalized
		{
			name:         `\" in file vs " in find`,
			content:      "echo \\\"brew install failed\\\"",
			find:         `echo "brew install failed"`,
			wantText:     "echo \\\"brew install failed\\\"",
			wantStrategy: "escape-normalized",
		},
		{
			name:         `literal \n in find vs real newline`,
			content:      "line1\nline2",
			find:         `line1\nline2`,
			wantText:     "line1\nline2",
			wantStrategy: "escape-normalized",
		},
		{
			name:         `literal \t in find vs real tab`,
			content:      "hello\tworld",
			find:         `hello\tworld`,
			wantText:     "hello\tworld",
			wantStrategy: "escape-normalized",
		},
		{
			name:         `\$ in find vs $ in file`,
			content:      "echo $HOME",
			find:         `echo \$HOME`,
			wantText:     "echo $HOME",
			wantStrategy: "escape-normalized",
		},

		// Ambiguity
		{
			name:    "ambiguous exact duplicate",
			content: "foo\nbar\nfoo\nbaz",
			find:    "foo",
			wantErr: "multiple locations",
		},
		{
			name:    "ambiguous escaped duplicate",
			content: "echo \\\"hi\\\"\necho \\\"hi\\\"",
			find:    `echo "hi"`,
			wantErr: "multiple locations",
		},

		// Not found
		{
			name:    "not found",
			content: "hello world",
			find:    "goodbye universe",
			wantErr: "old text not found",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, err := FindMatch(tc.content, tc.find)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			if tc.wantText != "" {
				assert.Equal(t, tc.wantText, m.SearchText)
			}
			assert.Equal(t, tc.wantStrategy, m.Strategy)
		})
	}
}

func TestUnescapeString(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{`hello`, "hello"},
		{`hello\nworld`, "hello\nworld"},
		{`hello\tworld`, "hello\tworld"},
		{`echo \"hi\"`, `echo "hi"`},
		{`echo \'hi\'`, `echo 'hi'`},
		{`path\\to\\file`, `path\to\file`},
		{`\$HOME`, "$HOME"},
		{`hello\xworld`, `hello\xworld`}, // unknown escape preserved
	} {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, unescapeString(tc.in))
		})
	}
}

// Reproduce the exact Dockerfile scenario that motivated this change.
func TestFindMatch_DockerfileBrewScenario(t *testing.T) {
	t.Parallel()

	// File on disk has literal \" around "brew install failed".
	fileContent := strings.Join([]string{
		`RUN if [ "${INSTALL_BREW}" = "1" ]; then \`,
		`  if ! id -u linuxbrew >/dev/null 2>&1; then useradd -m -s /bin/bash linuxbrew; fi; \`,
		`  mkdir -p "${BREW_INSTALL_DIR}"; \`,
		`  chown -R linuxbrew:linuxbrew "$(dirname "${BREW_INSTALL_DIR}")"; \`,
		`  su - linuxbrew -c "NONINTERACTIVE=1 CI=1 /bin/bash -c '$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)'"; \`,
		`  if [ ! -e "${BREW_INSTALL_DIR}/Library" ]; then ln -s "${BREW_INSTALL_DIR}/Homebrew/Library" "${BREW_INSTALL_DIR}/Library"; fi; \`,
		`  if [ ! -x "${BREW_INSTALL_DIR}/bin/brew" ]; then echo \"brew install failed\"; exit 1; fi; \`,
		`  ln -sf "${BREW_INSTALL_DIR}/bin/brew" /usr/local/bin/brew; \`,
		`fi`,
	}, "\n")

	// LLM dropped the backslashes: \" → "
	llmOldText := strings.Join([]string{
		`RUN if [ "${INSTALL_BREW}" = "1" ]; then \`,
		`  if ! id -u linuxbrew >/dev/null 2>&1; then useradd -m -s /bin/bash linuxbrew; fi; \`,
		`  mkdir -p "${BREW_INSTALL_DIR}"; \`,
		`  chown -R linuxbrew:linuxbrew "$(dirname "${BREW_INSTALL_DIR}")"; \`,
		`  su - linuxbrew -c "NONINTERACTIVE=1 CI=1 /bin/bash -c '$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)'"; \`,
		`  if [ ! -e "${BREW_INSTALL_DIR}/Library" ]; then ln -s "${BREW_INSTALL_DIR}/Homebrew/Library" "${BREW_INSTALL_DIR}/Library"; fi; \`,
		`  if [ ! -x "${BREW_INSTALL_DIR}/bin/brew" ]; then echo "brew install failed"; exit 1; fi; \`,
		`  ln -sf "${BREW_INSTALL_DIR}/bin/brew" /usr/local/bin/brew; \`,
		`fi`,
	}, "\n")

	m, err := FindMatch(fileContent, llmOldText)
	require.NoError(t, err)
	assert.Equal(t, fileContent, m.SearchText)
	assert.Equal(t, "escape-normalized", m.Strategy)
	assert.Equal(t, "REPLACED", strings.Replace(fileContent, m.SearchText, "REPLACED", 1))
}
