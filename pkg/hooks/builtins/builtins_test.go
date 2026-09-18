package builtins_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/portcullis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/internal/portcullistest"
)

// TestRegisterInstallsAllBuiltins pins the public contract of [Register]:
// every name documented in the package constants must be resolvable on
// the registry after registration. If a future change adds or renames a
// builtin without updating Register, this test fails.
func TestRegisterInstallsAllBuiltins(t *testing.T) {
	t.Parallel()

	r := hooks.NewRegistry()
	require.NoError(t, builtins.Register(r))

	for _, name := range []string{
		builtins.AddContext,
		builtins.AddDate,
		builtins.AddEnvironmentInfo,
		builtins.AddPromptFiles,
		builtins.AddGitStatus,
		builtins.AddGitDiff,
		builtins.AddDirectoryListing,
		builtins.AddUserInfo,
		builtins.AddRecentCommits,
		builtins.MaxIterations,
		builtins.RedactSecrets,
		builtins.LimitLargeToolResults,
		builtins.HTTPPost,
		builtins.Unload,
	} {
		fn, ok := r.LookupBuiltin(name)
		assert.True(t, ok, "builtin %q must be registered", name)
		assert.NotNil(t, fn, "builtin %q must have a non-nil function", name)
	}
}

// TestAddDateReturnsTodaysDate verifies the date builtin emits independently
// diffable date context.
func TestAddDateReturnsTodaysDate(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.AddDate)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s"}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput)
	assert.Equal(t, hooks.EventTurnStart, out.HookSpecificOutput.HookEventName,
		"add_date must target turn_start, not session_start")
	require.Len(t, out.HookSpecificOutput.InstructionContext, 1)
	assert.Equal(t, "core/date", out.HookSpecificOutput.InstructionContext[0].Key)
	assert.Contains(t, out.HookSpecificOutput.InstructionContext[0].Content, time.Now().Format("2006-01-02"))
}

// TestAddEnvironmentInfoUsesInputCwd verifies that the env-info builtin
// reads its working directory from the Input (not from os.Getwd) and
// emits a session_start AdditionalContext that reflects that path. We
// assert on the Cwd appearing verbatim rather than the full env block
// format, to stay stable across cosmetic tweaks to GetEnvironmentInfo.
func TestAddEnvironmentInfoUsesInputCwd(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.AddEnvironmentInfo)

	cwd := t.TempDir()
	out, err := fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: cwd}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput)
	assert.Equal(t, hooks.EventSessionStart, out.HookSpecificOutput.HookEventName,
		"add_environment_info must target session_start, not turn_start")
	assert.Contains(t, out.HookSpecificOutput.AdditionalContext, cwd,
		"env info must reflect the Input's Cwd, not os.Getwd")
}

// TestAddEnvironmentInfoNoCwdIsNoop documents the safety behavior: with
// an empty Cwd the builtin contributes nothing rather than fabricating
// info from os.Getwd or "<unknown>". Returning a nil Output is a valid
// successful no-op per the BuiltinFunc contract.
func TestAddEnvironmentInfoNoCwdIsNoop(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.AddEnvironmentInfo)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s"}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)

	out, err = fn(t.Context(), nil, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

// TestAddPromptFilesReadsFromCwd verifies that add_prompt_files includes
// both the resolved source path and its contents.
func TestAddPromptFilesReadsFromCwd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const promptBody = "Project guidelines: prefer Go."
	require.NoError(t, os.WriteFile(filepath.Join(dir, "PROMPT.md"), []byte(promptBody), 0o600))

	fn := lookup(t, builtins.AddPromptFiles)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: dir}, []string{"PROMPT.md"})
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput)
	assert.Equal(t, hooks.EventTurnStart, out.HookSpecificOutput.HookEventName,
		"add_prompt_files must target turn_start, not session_start")
	require.Len(t, out.HookSpecificOutput.InstructionContext, 1)
	source := out.HookSpecificOutput.InstructionContext[0]
	assert.Contains(t, source.Key, "core/prompt-file-")
	assert.True(t, source.CompleteGroup)
	assert.Contains(t, source.Content, "Instructions from: "+filepath.Join(dir, "PROMPT.md"))
	assert.Contains(t, source.Content, promptBody)
	assert.Contains(t, source.ChangedContent, "replace the previous instructions from that file")
	assert.Contains(t, source.ChangedContent, filepath.Join(dir, "PROMPT.md"))
}

// TestAddPromptFilesListsNestedFiles covers the monorepo case: the closest
// file is loaded in full while the ones below the working dir are listed by
// path only, as a separate instruction source.
func TestAddPromptFilesListsNestedFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "PROMPT.md"), []byte("root rules"), 0o600))
	nested := filepath.Join(dir, "service")
	require.NoError(t, os.Mkdir(nested, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(nested, "PROMPT.md"), []byte("service rules"), 0o600))

	fn := lookup(t, builtins.AddPromptFiles)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: dir}, []string{"--depth=1", "PROMPT.md"})
	require.NoError(t, err)
	require.NotNil(t, out)
	require.Len(t, out.HookSpecificOutput.InstructionContext, 2)

	loaded := out.HookSpecificOutput.InstructionContext[0]
	assert.Contains(t, loaded.Content, "root rules")
	assert.True(t, loaded.CompleteGroup)

	index := out.HookSpecificOutput.InstructionContext[1]
	assert.Equal(t, "core/prompt-file-index", index.Key)
	assert.Contains(t, index.Content, "service/PROMPT.md")
	assert.NotContains(t, index.Content, "service rules", "nested files are listed, not read")
}

// TestAddPromptFilesDepthArgIsNotAFilename pins that the --depth= argument is
// consumed as an option: a stray file named after it must not be looked up.
func TestAddPromptFilesDepthArgIsNotAFilename(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "PROMPT.md"), []byte("root rules"), 0o600))

	fn := lookup(t, builtins.AddPromptFiles)

	out, err := fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: dir}, []string{"--depth=1", "PROMPT.md"})
	require.NoError(t, err)
	require.Len(t, out.HookSpecificOutput.InstructionContext, 1,
		"only PROMPT.md resolves; --depth=1 is an option and nothing is nested")
}

// TestAddPromptFilesUnusableDepthIsIgnored documents that a malformed or
// negative depth degrades to "no nested listing" instead of failing the turn.
func TestAddPromptFilesUnusableDepthIsIgnored(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "PROMPT.md"), []byte("root rules"), 0o600))
	nested := filepath.Join(dir, "service")
	require.NoError(t, os.Mkdir(nested, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(nested, "PROMPT.md"), []byte("service rules"), 0o600))

	fn := lookup(t, builtins.AddPromptFiles)

	for _, arg := range []string{"--depth=-1", "--depth=deep", "--depth="} {
		out, err := fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: dir}, []string{arg, "PROMPT.md"})
		require.NoError(t, err)
		assert.Len(t, out.HookSpecificOutput.InstructionContext, 1, arg)
	}
}

// TestAddPromptFilesMissingFileIsTolerated documents that missing configured
// files do not prevent surviving files from contributing.
func TestAddPromptFilesMissingFileIsTolerated(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const promptBody = "still here"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "OK.md"), []byte(promptBody), 0o600))

	fn := lookup(t, builtins.AddPromptFiles)

	// One missing + one good: the good one survives.
	out, err := fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: dir}, []string{"MISSING.md", "OK.md"})
	require.NoError(t, err)
	require.NotNil(t, out)
	require.Len(t, out.HookSpecificOutput.InstructionContext, 1)
	assert.Contains(t, out.HookSpecificOutput.InstructionContext[0].Content, promptBody)

	out, err = fn(t.Context(), &hooks.Input{SessionID: "s", Cwd: dir}, []string{"MISSING.md"})
	require.NoError(t, err)
	require.Len(t, out.HookSpecificOutput.InstructionContext, 1)
	assert.True(t, out.HookSpecificOutput.InstructionContext[0].SetMarker)
	assert.True(t, out.HookSpecificOutput.InstructionContext[0].CompleteGroup)
}

// TestAddPromptFilesNoArgsIsNoop pins the early-return behavior: with
// no args (or empty Cwd, or nil Input) the builtin does nothing rather
// than returning an empty AdditionalContext that would still register
// as a contribution.
func TestAddPromptFilesNoArgsIsNoop(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.AddPromptFiles)

	cases := []struct {
		name string
		in   *hooks.Input
		args []string
	}{
		{"nil input", nil, []string{"PROMPT.md"}},
		{"empty cwd", &hooks.Input{SessionID: "s"}, []string{"PROMPT.md"}},
		{"empty args", &hooks.Input{SessionID: "s", Cwd: t.TempDir()}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := fn(t.Context(), tc.in, tc.args)
			require.NoError(t, err)
			assert.Nil(t, out)
		})
	}
}

// lookup registers the builtins on a fresh Registry and returns the
// named BuiltinFunc, failing the test if it isn't present. Centralising
// the boilerplate keeps the per-builtin tests focused on behavior.
//
// It injects an SSRF-unsafe HTTP client for http_post so tests can reach
// httptest.NewServer (which binds to 127.0.0.1); production wiring uses the
// safe dial-time-protected client.
func lookup(t *testing.T, name string) hooks.BuiltinFunc {
	t.Helper()
	r := hooks.NewRegistry()
	require.NoError(t, builtins.Register(r,
		builtins.WithHTTPPostClient(httpclient.NewSafeClient(30*time.Second, true))))
	fn, ok := r.LookupBuiltin(name)
	require.True(t, ok, "builtin %q must be registered", name)
	require.NotNil(t, fn)
	return fn
}

// TestApplyAgentDefaultsAlwaysInjectsLargeResultLimiter pins that the runtime
// always mounts the large-result limiter as a tool_response_transform hook.
func TestApplyAgentDefaultsAlwaysInjectsLargeResultLimiter(t *testing.T) {
	t.Parallel()

	cfg := builtins.ApplyAgentDefaults(nil, builtins.AgentDefaults{})
	require.NotNil(t, cfg)
	require.Len(t, cfg.ToolResponseTransform, 1)
	assert.Equal(t, "*", cfg.ToolResponseTransform[0].Matcher)
	require.Len(t, cfg.ToolResponseTransform[0].Hooks, 1)
	assert.Equal(t, builtins.LimitLargeToolResults, cfg.ToolResponseTransform[0].Hooks[0].Command)
	assert.Equal(t, hooks.HookTypeBuiltin, cfg.ToolResponseTransform[0].Hooks[0].Type)
	require.Len(t, cfg.SessionEnd, 1)
	assert.Equal(t, builtins.LimitLargeToolResults, cfg.SessionEnd[0].Command)
}

// TestApplyAgentDefaultsPromptFilesDepth pins how the nested-scan depth
// reaches the builtin: as a leading --depth= argument, so the hook's args
// stay a flat []string.
func TestApplyAgentDefaultsPromptFilesDepth(t *testing.T) {
	t.Parallel()

	cfg := builtins.ApplyAgentDefaults(nil, builtins.AgentDefaults{
		AddPromptFiles:      []string{"PROMPT.md"},
		AddPromptFilesDepth: 2,
	})
	require.NotNil(t, cfg)
	require.Len(t, cfg.TurnStart, 1)
	assert.Equal(t, []string{"--depth=2", "PROMPT.md"}, cfg.TurnStart[0].Args)
}

// TestApplyAgentDefaultsInjectsExpectedEvents verifies which event each
// flag targets — turn_start for date / prompt files (recompute every
// turn), session_start for environment info (cwd / OS / arch are
// session-stable). Regressing this would silently change when users
// see today's date.
func TestApplyAgentDefaultsInjectsExpectedEvents(t *testing.T) {
	t.Parallel()

	cfg := builtins.ApplyAgentDefaults(nil, builtins.AgentDefaults{
		AddDate:            true,
		AddEnvironmentInfo: true,
		AddPromptFiles:     []string{"PROMPT.md"},
	})
	require.NotNil(t, cfg)

	require.Len(t, cfg.TurnStart, 2, "add_date and add_prompt_files must inject turn_start hooks")
	assert.Equal(t, builtins.AddDate, cfg.TurnStart[0].Command)
	assert.Equal(t, hooks.HookTypeBuiltin, cfg.TurnStart[0].Type)
	assert.Equal(t, builtins.AddPromptFiles, cfg.TurnStart[1].Command)
	assert.Equal(t, []string{"PROMPT.md"}, cfg.TurnStart[1].Args)

	require.Len(t, cfg.SessionStart, 1, "add_environment_info must inject a session_start hook")
	assert.Equal(t, builtins.AddEnvironmentInfo, cfg.SessionStart[0].Command)

	require.Len(t, cfg.ToolResponseTransform, 1, "large-result limiter must always be injected")
	require.Len(t, cfg.ToolResponseTransform[0].Hooks, 1)
	assert.Equal(t, builtins.LimitLargeToolResults, cfg.ToolResponseTransform[0].Hooks[0].Command)
	require.Len(t, cfg.SessionEnd, 1, "large-result cleanup must always be injected")
	assert.Equal(t, builtins.LimitLargeToolResults, cfg.SessionEnd[0].Command)
}

func TestLimitLargeToolResultsStoresFullOutputAndReturnsTailNotice(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	var b strings.Builder
	for i := range 3000 {
		b.WriteString(strings.Repeat("x", 600))
		b.WriteString(" line ")
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	original := b.String()

	fn := lookup(t, builtins.LimitLargeToolResults)
	out, err := fn(t.Context(), &hooks.Input{
		HookEventName: hooks.EventToolResponseTransform,
		ToolCategory:  "filesystem",
		ToolResponse:  original,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse)

	updated := *out.HookSpecificOutput.UpdatedToolResponse
	assert.Contains(t, updated, "Tool call result was too large")
	assert.Contains(t, updated, "The full result is available in a file:")
	assert.Contains(t, updated, "Showing the last")
	assert.NotContains(t, updated, strings.Repeat("x", 600)+" line 0\n")
	assert.Contains(t, updated, " line 2999\n")

	stored, err := os.ReadFile(extractLargeResultPath(t, updated))
	require.NoError(t, err)
	assert.Equal(t, original, string(stored))
}

func TestLimitLargeToolResultsPreservesUTF8Tail(t *testing.T) {
	t.Parallel()

	payload := strings.Repeat("世", maxToolCallResultBytesForTest/len("世")+largeToolCallResultTailBytesForTest/len("世")+10)

	fn := lookup(t, builtins.LimitLargeToolResults)
	out, err := fn(t.Context(), &hooks.Input{
		SessionID:     "utf8-session",
		HookEventName: hooks.EventToolResponseTransform,
		ToolCategory:  "shell",
		ToolResponse:  payload,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse)
	updated := *out.HookSpecificOutput.UpdatedToolResponse
	assert.Contains(t, updated, "世")
	assert.Equal(t, updated, strings.ToValidUTF8(updated, ""))
}

func TestLimitLargeToolResultsCleansSessionTempDir(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	fn := lookup(t, builtins.LimitLargeToolResults)
	out, err := fn(t.Context(), &hooks.Input{
		SessionID:     "cleanup/session",
		HookEventName: hooks.EventToolResponseTransform,
		ToolCategory:  "filesystem",
		ToolResponse:  strings.Repeat("x", maxToolCallResultBytesForTest+1),
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	path := extractLargeResultPath(t, *out.HookSpecificOutput.UpdatedToolResponse)
	_, err = os.Stat(path)
	require.NoError(t, err)

	_, err = fn(t.Context(), &hooks.Input{
		SessionID:     "cleanup/session",
		HookEventName: hooks.EventSessionEnd,
	}, nil)
	require.NoError(t, err)
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "session_end must clean up large-result temp files")
}

const (
	maxToolCallResultBytesForTest       = 50 * 1024
	largeToolCallResultTailBytesForTest = 50 * 1024
)

func TestLimitLargeToolResultsNoopsForInternalCategory(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.LimitLargeToolResults)
	out, err := fn(t.Context(), &hooks.Input{
		HookEventName: hooks.EventToolResponseTransform,
		ToolCategory:  "memory",
		ToolResponse:  strings.Repeat("x", maxToolCallResultBytesForTest+1),
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestLimitLargeToolResultsCapsExternalToolCategories(t *testing.T) {
	for _, category := range []string{"mcp", "a2a"} {
		t.Run(category, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())

			fn := lookup(t, builtins.LimitLargeToolResults)
			out, err := fn(t.Context(), &hooks.Input{
				SessionID:     category + "-session",
				HookEventName: hooks.EventToolResponseTransform,
				ToolCategory:  category,
				ToolResponse:  strings.Repeat("x", maxToolCallResultBytesForTest+1),
			}, nil)
			require.NoError(t, err)
			require.NotNil(t, out)
			require.NotNil(t, out.HookSpecificOutput)
			require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse)
			assert.Contains(t, *out.HookSpecificOutput.UpdatedToolResponse, "Tool call result was too large")
		})
	}
}

func TestLimitLargeToolResultsTriggersOnLineCount(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	payload := strings.Repeat("x\n", 2001)
	require.LessOrEqual(t, len(payload), maxToolCallResultBytesForTest)

	fn := lookup(t, builtins.LimitLargeToolResults)
	out, err := fn(t.Context(), &hooks.Input{
		SessionID:     "line-count-session",
		HookEventName: hooks.EventToolResponseTransform,
		ToolCategory:  "shell",
		ToolResponse:  payload,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse)
	updated := *out.HookSpecificOutput.UpdatedToolResponse
	assert.Contains(t, updated, "Tool call result was too large")
	assert.NotContains(t, updated, strings.Repeat("x\n", 2001))
}

func TestLimitLargeToolResultsNoopsForSmallOutput(t *testing.T) {
	t.Parallel()

	fn := lookup(t, builtins.LimitLargeToolResults)
	out, err := fn(t.Context(), &hooks.Input{
		HookEventName: hooks.EventToolResponseTransform,
		ToolCategory:  "shell",
		ToolResponse:  "small output",
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

// TestLimitLargeToolResultsReadFileKeepsHeadWithRangedReadNotice is a
// regression test for issue #3889: a truncated read_file result must keep
// the head of the file (front matter, imports, ...) instead of the tail,
// and the notice must tell the model how to fetch the rest with a ranged
// read_file call.
func TestLimitLargeToolResultsReadFileKeepsHeadWithRangedReadNotice(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	var b strings.Builder
	for i := range 3000 {
		b.WriteString(strings.Repeat("x", 600))
		b.WriteString(" line ")
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	original := b.String()

	fn := lookup(t, builtins.LimitLargeToolResults)
	out, err := fn(t.Context(), &hooks.Input{
		SessionID:     "read-file-head-session",
		HookEventName: hooks.EventToolResponseTransform,
		ToolCategory:  "filesystem",
		ToolName:      "read_file",
		ToolInput:     map[string]any{"path": "big.txt"},
		ToolResponse:  original,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse)

	updated := *out.HookSpecificOutput.UpdatedToolResponse
	assert.Contains(t, updated, "Tool call result was too large")
	assert.Contains(t, updated, "Showing the first")
	assert.Contains(t, updated, "call read_file again")
	assert.Contains(t, updated, `"limit"`)

	// Head preserved, tail dropped — the opposite of the shell/tail case.
	head := extractShownExcerpt(t, updated)
	assert.True(t, strings.HasPrefix(head, strings.Repeat("x", 600)+" line 0\n"),
		"head excerpt must start at the beginning of the result")
	assert.NotContains(t, updated, " line 2999\n")

	// The suggested continuation line is 1 (start of an unranged read)
	// plus the number of complete lines shown.
	assert.Contains(t, updated, fmt.Sprintf(`"line": %d`, 1+strings.Count(head, "\n")))

	// The full result is still spilled for recovery.
	stored, err := os.ReadFile(extractLargeResultPath(t, updated))
	require.NoError(t, err)
	assert.Equal(t, original, string(stored))
}

// TestLimitLargeToolResultsReadFileContinuationRespectsRequestedStartLine
// verifies that when the truncated read_file call itself used a line offset,
// the suggested continuation line is absolute in the file, not relative to
// the returned range.
func TestLimitLargeToolResultsReadFileContinuationRespectsRequestedStartLine(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	payload := strings.Repeat(strings.Repeat("y", 600)+"\n", 3000)

	fn := lookup(t, builtins.LimitLargeToolResults)
	out, err := fn(t.Context(), &hooks.Input{
		SessionID:     "read-file-offset-session",
		HookEventName: hooks.EventToolResponseTransform,
		ToolCategory:  "filesystem",
		ToolName:      "read_file",
		ToolInput:     map[string]any{"path": "big.txt", "line": float64(101)},
		ToolResponse:  payload,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse)

	updated := *out.HookSpecificOutput.UpdatedToolResponse
	head := extractShownExcerpt(t, updated)
	assert.Contains(t, updated, fmt.Sprintf(`"line": %d`, 101+strings.Count(head, "\n")))
}

// TestLimitLargeToolResultsReadFileSingleLongLineDoesNotSuggestLoopingRead
// covers the pathological case for the head notice: the bounded head cuts
// inside a single line longer than the byte cap, so it contains no complete
// line and a "line"/"limit" continuation would restart at the same line
// forever. The notice must state that line-based continuation cannot
// advance within the first line instead of suggesting such a call.
func TestLimitLargeToolResultsReadFileSingleLongLineDoesNotSuggestLoopingRead(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	original := "line1-start " + strings.Repeat("z", maxToolCallResultBytesForTest+largeToolCallResultTailBytesForTest)
	require.NotContains(t, original, "\n", "payload must be a single line")

	fn := lookup(t, builtins.LimitLargeToolResults)
	out, err := fn(t.Context(), &hooks.Input{
		SessionID:     "read-file-long-line-session",
		HookEventName: hooks.EventToolResponseTransform,
		ToolCategory:  "filesystem",
		ToolName:      "read_file",
		ToolInput:     map[string]any{"path": "big.txt"},
		ToolResponse:  original,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse)

	updated := *out.HookSpecificOutput.UpdatedToolResponse
	assert.Contains(t, updated, "Tool call result was too large")
	assert.Contains(t, updated, fmt.Sprintf("Showing the first %d bytes", largeToolCallResultTailBytesForTest))

	// No line-based continuation suggestion: any "line": N (including the
	// misleading "line": 1) would re-read the same oversized line forever.
	assert.NotContains(t, updated, `"line":`)
	assert.NotContains(t, updated, "call read_file again")
	assert.Contains(t, updated, "first line")
	assert.Contains(t, updated, "cannot advance")

	// The head excerpt is retained verbatim and stays valid UTF-8.
	head := extractShownExcerpt(t, updated)
	assert.Equal(t, original[:largeToolCallResultTailBytesForTest], head)
	assert.Equal(t, updated, strings.ToValidUTF8(updated, ""))

	// The full result is still spilled for recovery.
	stored, err := os.ReadFile(extractLargeResultPath(t, updated))
	require.NoError(t, err)
	assert.Equal(t, original, string(stored))
}

// TestLimitLargeToolResultsReadFileHeadPreservesUTF8 mirrors the tail
// UTF-8 test: a byte-boundary cut of the head must not leave a dangling
// partial rune.
func TestLimitLargeToolResultsReadFileHeadPreservesUTF8(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	payload := strings.Repeat("世", maxToolCallResultBytesForTest/len("世")+largeToolCallResultTailBytesForTest/len("世")+10)

	fn := lookup(t, builtins.LimitLargeToolResults)
	out, err := fn(t.Context(), &hooks.Input{
		SessionID:     "read-file-utf8-session",
		HookEventName: hooks.EventToolResponseTransform,
		ToolCategory:  "filesystem",
		ToolName:      "read_file",
		ToolResponse:  payload,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse)
	updated := *out.HookSpecificOutput.UpdatedToolResponse
	assert.Contains(t, updated, "世")
	assert.Equal(t, updated, strings.ToValidUTF8(updated, ""))
}

// TestLimitLargeToolResultsShellKeepsTail pins that head-first truncation is
// scoped to read_file: shell output keeps its tail, where exit diagnostics
// live.
func TestLimitLargeToolResultsShellKeepsTail(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	var b strings.Builder
	for i := range 3000 {
		b.WriteString(strings.Repeat("x", 600))
		b.WriteString(" line ")
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}

	fn := lookup(t, builtins.LimitLargeToolResults)
	out, err := fn(t.Context(), &hooks.Input{
		SessionID:     "shell-tail-session",
		HookEventName: hooks.EventToolResponseTransform,
		ToolCategory:  "shell",
		ToolName:      "shell",
		ToolResponse:  b.String(),
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse)

	updated := *out.HookSpecificOutput.UpdatedToolResponse
	assert.Contains(t, updated, "Showing the last")
	assert.Contains(t, updated, " line 2999\n")
	assert.NotContains(t, updated, " line 0\n")
}

// TestLimitLargeToolResultsMCPReadFileKeepsTail pins that head-first
// truncation requires the built-in filesystem category, not just the
// read_file name: an mcp/a2a tool that happens to be called read_file has
// no known line/limit contract, so it keeps the generic tail notice with
// no local ranged-read advice.
func TestLimitLargeToolResultsMCPReadFileKeepsTail(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	var b strings.Builder
	for i := range 3000 {
		b.WriteString(strings.Repeat("x", 600))
		b.WriteString(" line ")
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}

	fn := lookup(t, builtins.LimitLargeToolResults)
	out, err := fn(t.Context(), &hooks.Input{
		SessionID:     "mcp-read-file-session",
		HookEventName: hooks.EventToolResponseTransform,
		ToolCategory:  "mcp",
		ToolName:      "read_file",
		ToolResponse:  b.String(),
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput.UpdatedToolResponse)

	updated := *out.HookSpecificOutput.UpdatedToolResponse
	assert.Contains(t, updated, "Showing the last")
	assert.Contains(t, updated, " line 2999\n")
	assert.NotContains(t, updated, " line 0\n")
	assert.NotContains(t, updated, "Showing the first")
	assert.NotContains(t, updated, "call read_file again")
	assert.NotContains(t, updated, `"line":`)
}

func extractLargeResultPath(t *testing.T, response string) string {
	t.Helper()
	const marker = "The full result is available in a file: "
	idx := strings.Index(response, marker)
	require.NotEqual(t, -1, idx)
	pathStart := idx + len(marker)
	pathEnd := strings.Index(response[pathStart:], "\n")
	require.NotEqual(t, -1, pathEnd)
	return response[pathStart : pathStart+pathEnd]
}

// extractShownExcerpt returns the excerpt following the limiter's notice,
// which ends with ":\n\n" in both the head and tail message formats.
func extractShownExcerpt(t *testing.T, response string) string {
	t.Helper()
	const sep = ":\n\n"
	idx := strings.Index(response, sep)
	require.NotEqual(t, -1, idx)
	return response[idx+len(sep):]
}

// TestRegisterSnapshotInstallsBuiltin verifies that the dedicated
// snapshot entry point installs the snapshot builtin and returns a
// controller wired up to the registered hook.
func TestRegisterSnapshotInstallsBuiltin(t *testing.T) {
	t.Parallel()

	r := hooks.NewRegistry()
	ctrl, err := builtins.RegisterSnapshot(r, true)
	require.NoError(t, err)
	require.NotNil(t, ctrl)
	assert.True(t, ctrl.Enabled())

	fn, ok := r.LookupBuiltin(builtins.Snapshot)
	assert.True(t, ok, "snapshot must be registered by RegisterSnapshot")
	assert.NotNil(t, fn)
}

// TestRegisterSnapshotDisabledStillExposesController verifies that an
// embedder can install the snapshot builtin without auto-injection, in
// which case the controller still exists (so /undo etc. work for hooks
// the user wired manually) but Enabled() reports false.
func TestRegisterSnapshotDisabledStillExposesController(t *testing.T) {
	t.Parallel()

	r := hooks.NewRegistry()
	ctrl, err := builtins.RegisterSnapshot(r, false)
	require.NoError(t, err)
	require.NotNil(t, ctrl)
	assert.False(t, ctrl.Enabled())

	_, ok := r.LookupBuiltin(builtins.Snapshot)
	assert.True(t, ok)
}

// TestSnapshotControllerAutoInjectWiresFourEvents verifies that the
// controller's AutoInject mounts the snapshot hook on session_start,
// turn_start, turn_end, and session_end — the four boundaries needed
// to bracket every session and every turn. Per-tool capture stays
// opt-in via YAML.
func TestSnapshotControllerAutoInjectWiresFourEvents(t *testing.T) {
	t.Parallel()

	r := hooks.NewRegistry()
	ctrl, err := builtins.RegisterSnapshot(r, true)
	require.NoError(t, err)

	inj, ok := ctrl.(builtins.AutoInjector)
	require.True(t, ok, "controller must satisfy AutoInjector")

	cfg := &hooks.Config{}
	inj.AutoInject(cfg)
	require.Len(t, cfg.SessionStart, 1)
	require.Len(t, cfg.TurnStart, 1)
	require.Len(t, cfg.TurnEnd, 1)
	require.Len(t, cfg.SessionEnd, 1)
	assert.Equal(t, builtins.Snapshot, cfg.SessionStart[0].Command)
	assert.Equal(t, builtins.Snapshot, cfg.TurnStart[0].Command)
	assert.Equal(t, builtins.Snapshot, cfg.TurnEnd[0].Command)
	assert.Equal(t, builtins.Snapshot, cfg.SessionEnd[0].Command)
}

// TestSnapshotControllerAutoInjectDisabledIsNoop verifies that a
// controller constructed with enabled=false makes no changes to cfg,
// so an embedder can pass it unconditionally to the runtime as an
// AutoInjector and rely on the bool to gate auto-injection.
func TestSnapshotControllerAutoInjectDisabledIsNoop(t *testing.T) {
	t.Parallel()

	r := hooks.NewRegistry()
	ctrl, err := builtins.RegisterSnapshot(r, false)
	require.NoError(t, err)

	inj, ok := ctrl.(builtins.AutoInjector)
	require.True(t, ok)

	cfg := &hooks.Config{}
	inj.AutoInject(cfg)
	assert.True(t, cfg.IsEmpty(), "disabled controller must not inject any hooks")
}

func TestApplyAgentDefaultsAppendsToUserHooks(t *testing.T) {
	t.Parallel()

	user := hooks.Hook{Type: hooks.HookTypeCommand, Command: "echo hi"}
	cfg := &hooks.Config{TurnStart: []hooks.Hook{user}}

	got := builtins.ApplyAgentDefaults(cfg, builtins.AgentDefaults{AddDate: true})
	require.NotNil(t, got)
	require.Len(t, got.TurnStart, 2)
	assert.Equal(t, user, got.TurnStart[0])
	assert.Equal(t, builtins.AddDate, got.TurnStart[1].Command)
}

func TestApplyAgentDefaultsOrdersTransformsBeforeLimiter(t *testing.T) {
	t.Parallel()

	for _, redact := range []bool{false, true} {
		cfg := builtins.ApplyAgentDefaults(&hooks.Config{
			ToolResponseTransform: []hooks.MatcherConfig{{Hooks: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: "custom"}}}},
		}, builtins.AgentDefaults{RedactSecrets: redact})
		var names []string
		for _, matcher := range cfg.ToolResponseTransform {
			for _, hook := range matcher.Hooks {
				names = append(names, hook.Command)
			}
		}
		want := []string{"custom"}
		if redact {
			want = append(want, builtins.RedactSecrets)
		}
		want = append(want, builtins.LimitLargeToolResults)
		assert.Equal(t, want, names)
	}
}

func TestTransformPipelineRedactsBeforeSpillingLargeOutput(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	secret := portcullistest.FakeGitHubPAT("cxLeRrvbJfmYdUtr70xnNE3Q7Gvli4")
	original := secret + "\n" + strings.Repeat("safe output\n", 6000) + secret + "\n"
	for _, manual := range []bool{false, true} {
		cfg := &hooks.Config{}
		if manual {
			cfg.ToolResponseTransform = []hooks.MatcherConfig{{Hooks: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: builtins.RedactSecrets}}}}
		}
		cfg = builtins.ApplyAgentDefaults(cfg, builtins.AgentDefaults{RedactSecrets: !manual})
		registry := hooks.NewRegistry()
		require.NoError(t, builtins.Register(registry))
		exec := hooks.NewExecutorWithRegistry(cfg, t.TempDir(), nil, registry)
		result, err := exec.Dispatch(t.Context(), hooks.EventToolResponseTransform, &hooks.Input{
			SessionID: "redaction-pipeline", ToolName: "shell", ToolCategory: "shell", ToolResponse: original,
		})
		require.NoError(t, err)
		require.NotNil(t, result.UpdatedToolResponse)
		updated := *result.UpdatedToolResponse
		assert.Contains(t, updated, "Tool call result was too large")
		assert.NotContains(t, updated, secret)
		assert.Contains(t, updated, portcullis.Marker)

		stored, err := os.ReadFile(extractLargeResultPath(t, updated))
		require.NoError(t, err)
		assert.Equal(t, portcullis.Redact(original), string(stored))
	}
}
