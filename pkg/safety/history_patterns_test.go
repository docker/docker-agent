package safety

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyCommand_HistoryReadPatterns(t *testing.T) {
	for _, command := range []string{
		`sed -n '10,20p' file.go`,
		`sed -n "10,20p" "path with spaces/file.go"`,
		`sed -n 10p ./file.go`,
		`sed -n '10,$p' 'path with spaces/file.go'`,
		`sed -n '1,10p'`,
		`head -50 file.go`,
		`head -50`,
		`head -c 100 file.go`,
		`head -c 100`,
		`head -n 50`,
		`tail -50 file.go`,
		`tail -50`,
		`tail -c 100 file.go`,
		`tail -c 100`,
		`tail -n 50`,
		`tail -n +10 file.go`,
		`tail -c +10 file.go`,
		`cat -n file.go other.go`,
		`ls -la pkg/ docs/`,
		`wc -lc file.go other.go`,
		`git branch --show-current`,
		`git branch -a`,
		`git branch -r`,
		`git branch --all`,
		`git branch --remotes`,
		`git branch --list`,
		`git branch -v`,
		`git branch -vv`,
		`git branch -avv`,
		`git ls-files`,
		`git ls-files --others --exclude-standard`,
		`git merge-base --fork-point origin/main HEAD`,
		`git rev-list --count HEAD`,
		`git worktree list`,
		`git worktree list --porcelain`,
		`git stash list`,
		`git stash list --oneline`,
		`gh pr view`,
		`gh pr view 42 --json title,body --jq .title`,
		`gh pr list --state open`,
		`gh pr diff 42`,
		`gh pr checks 42`,
		`gh pr status`,
		`gh issue view 42 --comments`,
		`gh issue list --limit 10`,
		`gh run view 42 --log-failed`,
		`gh run list --limit 10`,
		`gh run list -w ci.yml`,
		`gh run list -wci.yml`,
		`gh run list --workflow ci.yml`,
		`go env`,
		`go env GOPATH GOMOD`,
		`go env -json`,
		`go env --json`,
		`go version`,
		`go version -m ./bin/app`,
		`rg '\bfoo\b' pkg/`,
		`rg "\bfoo\b" pkg/`,
		`rg "func \w+" .`,
		`rg "\(foo\)" .`,
		`rg "foo\$" .`,
		`rg "a\"b" .`,
		`rg 'foo$' .`,
		`rg -n 'ClassifyCommand\(' .`,
		`rg '[a-z]{2}' .`,
		`git log --grep='fix$'`,
		`git log -S'func foo\(' --oneline`,
		`git diff -- 'pkg/*.go'`,
		`gh pr list --search "fix*"`,
	} {
		t.Run(command, func(t *testing.T) {
			assert.Equal(t, ClassSafe, ClassifyCommand(command).Class)
		})
	}
}

func TestClassifyCommand_HistoryReadEscapeHatches(t *testing.T) {
	for _, command := range []string{
		`sed -n 1w/tmp/outputp file`,
		`sed -n s/a/b/ep file`,
		`sed -n '1p;w /tmp/output' file`,
		`sed -n '1,10p' -i file`,
		`sed -n -e '1p' file`,
		`sed -n '1p' -i`,
		`sed -n '1p' '-i'`,
		`sed -n '1p' "-i"`,
		`sed -n '1p' $FLAGS`,
		`sed -n '1p' *`,
		`sed -n '1p' ./file${FLAGS}`,
		`sed -n "1,$p" file`,
		`sed -n 1,$p file`,
		`head -f file`,
		`tail -f file`,
		`tail -50 --follow`,
		`head -c --help file`,
		`git branch new-branch`,
		`git branch -m renamed`,
		`git branch -D old-branch`,
		`git rev-list --output=/tmp/output HEAD`,
		`git rev-list --output /tmp/output HEAD`,
		`git stash list --output=/tmp/output`,
		`git stash list --ext-diff -p`,
		`git stash list --textconv -p`,
		`git grep -Oeditor pattern`,
		`git -c core.pager=script stash list`,
		`gh pr view --web`,
		`gh pr view -w`,
		`gh pr view -cw`,
		`gh pr view --w''eb`,
		`gh pr view --w\eb`,
		`gh pr view $FLAGS`,
		`gh pr view --w${EMPTY}eb`,
		`gh pr checks --watch`,
		`gh issue view 42 --web`,
		`gh run view 42 -w`,
		`gh run view 42 -w=true`,
		`gh run view 42 -w''`,
		`rg --p\re=script pattern`,
		`rg "--pre=script" pattern`,
		`gh pr view "--w${EMPTY}eb"`,
		`gh pr view *`,
		`gh pr view {--web,42}`,
		`gh pr view ?`,
		`gh pr view [a-z]*`,
		`gh pr view "'" --w\eb`,
		`gh pr view '"' --w\eb`,
		`git rev-list * HEAD`,
		`git rev-list {--output=/tmp/output,HEAD}`,
		`rg * .`,
		`rg foo {--pre=script,.}`,
		`git log --ext-diff -p`,
		`git diff --textconv`,
		`git show --ext-diff`,
		`gh pr checkout 42`,
		`gh pr merge 42`,
		`gh run rerun 42`,
		`gh api repos/owner/repo -f name=other`,
		`go env -w GOPROXY=off`,
		`go env --w GOPROXY=off`,
		`go env --w=true GOPROXY=off`,
		`go env -u GOPROXY`,
		`go env --u GOPROXY`,
		`go env -"w" GOPROXY=off`,
		`go test ./...`,
		`task test`,
		`find -delete -name file`,
		`sed -n '1p' file && pwd`,
		`gh pr view 42 | cat`,
		`go version > version.txt`,
	} {
		t.Run(command, func(t *testing.T) {
			assert.NotEqual(t, ClassSafe, ClassifyCommand(command).Class)
		})
	}
}

func TestClassifyCommand_HistoryDestructivePatterns(t *testing.T) {
	for _, tt := range []struct {
		command  string
		severity string
		category string
	}{
		{`gofmt -w file.go`, "medium", "fs-modify"},
		{`gofmt -s -w file.go`, "medium", "fs-modify"},
		{`goimports -w file.go`, "medium", "fs-modify"},
		{`goimports -local example.com -w file.go`, "medium", "fs-modify"},
		{`go fmt ./...`, "medium", "fs-modify"},
		{`golangci-lint fmt`, "medium", "fs-modify"},
		{`perl -i -pe 's/a/b/' file`, "medium", "fs-modify"},
		{`perl -pi -e 's/a/b/' file`, "medium", "fs-modify"},
		{`perl -pi.bak -e 's/a/b/' file`, "medium", "fs-modify"},
		{`perl -ipe 's/a/b/' file`, "medium", "fs-modify"},
		{`tee output.txt`, "medium", "fs-overwrite"},
		{`printf data | tee -a output.txt`, "medium", "fs-overwrite"},
		{`git push origin main -f`, "medium", "git-history"},
		{`git push origin main --force`, "medium", "git-history"},
		{`git push origin main --force-with-lease`, "medium", "git-history"},
		{`git push origin main --force-with-lease=main:abc123`, "medium", "git-history"},
		{`git push --delete origin old`, "medium", "git-history"},
		{`git push origin --delete old`, "medium", "git-history"},
		{`git push -d origin old`, "medium", "git-history"},
		{`git push origin -d old`, "medium", "git-history"},
		{`git checkout -f branch`, "high", "git-discard"},
		{`git checkout --force branch`, "high", "git-discard"},
		{`git checkout --ours file`, "medium", "git-discard"},
		{`git checkout --theirs file`, "medium", "git-discard"},
		{`git worktree remove ../old`, "medium", "git-discard"},
		{`git clean -fdq`, "high", "git-discard"},
		{`git clean -qf`, "high", "git-discard"},
		{`pkill -f server`, "medium", "proc-signal"},
		{`killall server`, "medium", "proc-signal"},
	} {
		t.Run(tt.command, func(t *testing.T) {
			label := ClassifyCommand(tt.command)
			assert.Equal(t, ClassDestructive, label.Class)
			assert.Equal(t, tt.severity, label.BlastRadius)
			assert.Equal(t, tt.category, label.Category)
		})
	}
}

func TestPatternToSafeRegexp_ConstrainedPlaceholders(t *testing.T) {
	for _, tt := range []struct {
		pattern string
		match   []string
		reject  []string
	}{
		{"<number>", []string{"0", "123"}, []string{"-f", "+1", "1e3", "abc", ""}},
		{"<sed-print>", []string{"1p", "1,10p", `'1,$p'`, `"1,10p"`}, []string{"1w/tmp/p", "s/a/b/ep", `"1,$p"`, "'1p\"", "1p;d"}},
		{"<read-path>", []string{"file.go", "./-i", "/tmp/file", "'file with spaces'", `"file with spaces"`}, []string{"-i", "'-i'", `"-i"`, "$FLAGS", "./${FLAGS}", "*", "./*", "''"}},
	} {
		t.Run(tt.pattern, func(t *testing.T) {
			re, err := regexp.Compile(patternToSafeRegexp(tt.pattern))
			require.NoError(t, err)
			for _, input := range tt.match {
				assert.True(t, re.MatchString(input), input)
			}
			for _, input := range tt.reject {
				assert.False(t, re.MatchString(input), input)
			}
		})
	}
}

func TestCarriesDenyFlag_ShortFlagsAndQuoting(t *testing.T) {
	for _, input := range []string{
		`--web`, `--web=true`, `-w`, `-cw`, `-wc`, `'--web'`, `--w''eb`, `--w\eb`, `$FLAGS`,
	} {
		assert.True(t, carriesDenyFlag(input, []string{"--web", "-w"}), input)
	}
	for _, input := range []string{`--json title`, `--repo owner/repo`, `-c`, `--state new`} {
		assert.False(t, carriesDenyFlag(input, []string{"--web", "-w"}), input)
	}
	assert.False(t, carriesDenyFlag(`$FLAGS`, nil))
}
