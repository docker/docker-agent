package safety

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassifyCommand_ShellSubstitutionIsNeverSafe(t *testing.T) {
	for _, command := range []string{
		`ls =(touch /tmp/marker)`,
		`cat (touch /tmp/marker)`,
		`wc =(touch /tmp/marker)`,
		`grep foo =(touch /tmp/marker)`,
		`git status =(touch /tmp/marker)`,
		`git ls-files =(touch /tmp/marker)`,
		`git worktree list =(touch /tmp/marker)`,
		`git merge-base =(touch /tmp/marker) HEAD`,
		`go version =(touch /tmp/marker)`,
		`ls 'quoted' =(touch /tmp/marker)`,
		`ls "'" =(touch /tmp/marker)`,
		`ls \" =(touch /tmp/marker)`,
		`ls "escaped\"quote" =(touch /tmp/marker)`,
		"ls\rtouch /tmp/marker",
	} {
		t.Run(command, func(t *testing.T) {
			assert.True(t, ContainsShellMetacharacter(command))
			assert.NotEqual(t, ClassSafe, ClassifyCommand(command).Class)
			assert.NotEqual(t, ClassSafe, LabelToolCall(BackgroundJobToolName, map[string]any{"cmd": command}, false, false).Class)
		})
	}
}

func TestContainsShellMetacharacter_LiteralParentheses(t *testing.T) {
	for _, command := range []string{
		`rg '\(foo\)' pkg/`,
		`rg "\(foo\)" pkg/`,
		`grep "'(" pkg/`,
		`grep '"(' pkg/`,
		`grep "escaped\"(quote" pkg/`,
		`ls 'file(1)'`,
		`ls file\(1\)`,
	} {
		t.Run(command, func(t *testing.T) {
			assert.False(t, ContainsShellMetacharacter(command))
			assert.Equal(t, ClassSafe, ClassifyCommand(command).Class)
		})
	}
}

func TestContainsFlagExpansion_QuotedEscapes(t *testing.T) {
	for _, command := range []string{
		`rg "\bfoo\b" pkg/`,
		`rg "\w+" pkg/`,
		`rg "escaped\"quote" pkg/`,
		`rg "foo\$" pkg/`,
	} {
		assert.False(t, containsFlagExpansion(command), command)
	}
	for _, command := range []string{
		`rg --p\re=script pattern`,
		`gh pr view "--w${EMPTY}eb"`,
		`gh pr view "escaped\"quote" $FLAGS`,
		`gh pr view "double\\" $FLAGS`,
		`gh pr view "unterminated\"`,
	} {
		assert.True(t, containsFlagExpansion(command), command)
	}
}
