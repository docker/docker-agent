package root

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	latestcfg "github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/input"
	"github.com/docker/docker-agent/pkg/leantui"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/profiling"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tui"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
	"github.com/docker/docker-agent/pkg/worktree"
)

// worktreeAutoName is the value stored when --worktree is given without an
// explicit name (cobra's NoOptDefVal). It also doubles as the reserved name a
// user can pass explicitly (--worktree=auto) to request a generated name; with
// cobra's optional-value flags the two are indistinguishable by design.
const worktreeAutoName = "auto"

type runExecFlags struct {
	agentName         string
	autoApprove       bool
	attachmentPath    string
	remoteAddress     string
	modelOverrides    []string
	promptFiles       []string
	dryRun            bool
	runConfig         config.RuntimeConfig
	sessionDB         string
	sessionID         string
	recordPath        string
	fakeResponses     string
	fakeStreamDelay   int
	exitAfterResponse bool
	cpuProfile        string
	memProfile        string
	forceTUI          bool
	sandbox           bool
	sandboxTemplate   string
	sbx               bool
	noKit             bool
	agentPickerSpec   string
	worktree          bool
	worktreeName      string
	worktreePR        string
	worktreeBase      string
	sessionReadOnly   bool

	// Exec only
	exec          bool
	hideToolCalls bool
	outputJSON    bool

	// Run only
	hideToolResults  bool
	lean             bool
	leanChanged      bool
	appName          string
	sidebar          bool
	listenAddr       string
	onEventSpecs     []string
	disabledCommands []string
	theme            string

	// globalPermissions holds the user-level global permission checker built
	// from user config settings. Nil when no global permissions are configured.
	globalPermissions *permissions.Checker
	snapshotsEnabled  bool

	// snapshotController is the [builtins.SnapshotController] for the
	// initial App: it is wired into the initial runtime as an
	// auto-injector and into the App via app.WithSnapshotController so
	// /undo, /snapshots, /reset drive the same instance that captures
	// the checkpoints. Sub-runtimes created by [createSessionSpawner]
	// build their own controller (and registry) so each spawned
	// session has independent snapshot state; that controller is local
	// to the spawner closure and never reaches this field.
	snapshotController builtins.SnapshotController
}

func newRunCmd() *cobra.Command {
	var flags runExecFlags

	cmd := &cobra.Command{
		Use:   "run [<agent-file>|<registry-ref>] [message]...",
		Short: "Run an agent",
		Long:  "Run an agent with the specified configuration and prompt",
		Example: `  docker-agent run ./agent.yaml
  docker-agent run ./team.yaml --agent root
  docker-agent run # built-in default agent
  docker-agent run coder # built-in coding agent
  docker-agent run ./echo.yaml "INSTRUCTIONS"
  docker-agent run ./echo.yaml "First question" "Follow-up question"
  echo "INSTRUCTIONS" | docker-agent run ./echo.yaml -
  docker-agent run ./agent.yaml --record  # Records session to auto-generated file`,
		GroupID:           "core",
		ValidArgsFunction: completeRunExec,
		Args:              cobra.ArbitraryArgs,
		RunE:              flags.runRunCommand,
	}

	addRunOrExecFlags(cmd, &flags)
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func addRunOrExecFlags(cmd *cobra.Command, flags *runExecFlags) {
	cmd.PersistentFlags().StringVarP(&flags.agentName, "agent", "a", "", "Name of the agent to run (defaults to the team's first agent)")
	cmd.PersistentFlags().BoolVar(&flags.autoApprove, "yolo", false, "Automatically approve all tool calls without prompting")
	cmd.PersistentFlags().BoolVar(&flags.hideToolResults, "hide-tool-results", false, "Hide tool call results")
	cmd.PersistentFlags().StringVar(&flags.attachmentPath, "attach", "", "Attach an image file to the message")
	cmd.PersistentFlags().StringArrayVar(&flags.promptFiles, "prompt-file", nil, "Append file contents to the prompt (repeatable)")
	cmd.PersistentFlags().StringArrayVar(&flags.modelOverrides, "model", nil, "Override agent model: [agent=]provider/model (repeatable)")
	cmd.PersistentFlags().BoolVar(&flags.dryRun, "dry-run", false, "Initialize the agent without executing anything")
	cmd.PersistentFlags().StringVar(&flags.remoteAddress, "remote", "", "Use remote runtime with specified address")
	cmd.PersistentFlags().StringVarP(&flags.sessionDB, "session-db", "s", filepath.Join(paths.GetHomeDir(), ".cagent", "session.db"), "Path to the session database")
	cmd.PersistentFlags().StringVar(&flags.sessionID, "session", "", "Continue from a previous session by ID or relative offset (e.g., -1 for last session). An explicit ID that does not exist yet is created with that ID.")
	cmd.PersistentFlags().StringVar(&flags.fakeResponses, "fake", "", "Replay AI responses from cassette file (for testing)")
	cmd.PersistentFlags().IntVar(&flags.fakeStreamDelay, "fake-stream", 0, "Simulate streaming with delay in ms between chunks (default 15ms if no value given)")
	cmd.Flag("fake-stream").NoOptDefVal = "15" // --fake-stream without value uses 15ms
	cmd.PersistentFlags().StringVar(&flags.recordPath, "record", "", "Record AI API interactions to cassette file (auto-generates filename if empty)")
	cmd.PersistentFlags().Lookup("record").NoOptDefVal = "true"
	cmd.PersistentFlags().BoolVar(&flags.exitAfterResponse, "exit-after-response", false, "Exit TUI after first assistant response completes")
	_ = cmd.PersistentFlags().MarkHidden("exit-after-response")
	cmd.PersistentFlags().StringVar(&flags.listenAddr, "listen", "", "Expose this run's control plane on the given address (e.g. 127.0.0.1:0)")
	_ = cmd.PersistentFlags().MarkHidden("listen")
	cmd.PersistentFlags().StringArrayVar(&flags.onEventSpecs, "on-event", nil, "Run shell command on event: --on-event <type>=<cmd> (or *=<cmd> for any). Repeatable.")
	cmd.PersistentFlags().StringVar(&flags.cpuProfile, "cpuprofile", "", "Write CPU profile to file")
	_ = cmd.PersistentFlags().MarkHidden("cpuprofile")
	cmd.PersistentFlags().StringVar(&flags.memProfile, "memprofile", "", "Write memory profile to file")
	_ = cmd.PersistentFlags().MarkHidden("memprofile")
	cmd.PersistentFlags().BoolVar(&flags.forceTUI, "force-tui", false, "Force TUI mode even when not in a terminal")
	_ = cmd.PersistentFlags().MarkHidden("force-tui")
	cmd.PersistentFlags().BoolVar(&flags.lean, "lean", false, "Use a simplified TUI with minimal chrome")
	cmd.PersistentFlags().StringVar(&flags.appName, "app-name", "", "Application name shown in the TUI in place of \"docker agent\"")
	cmd.PersistentFlags().StringSliceVar(&flags.disabledCommands, "disable-commands", nil, "Comma-separated list of slash commands to hide and disable in the TUI (e.g. /cost,/eval,/model)")
	cmd.PersistentFlags().BoolVar(&flags.sidebar, "sidebar", true, "Show the sidebar in the TUI (set --sidebar=false to hide it)")
	cmd.PersistentFlags().StringVar(&flags.theme, "theme", "", "Preselect a TUI theme by name (overrides the theme from user config; ignored outside the interactive TUI)")
	_ = cmd.RegisterFlagCompletionFunc("theme", completeTheme)
	cmd.PersistentFlags().BoolVar(&flags.sandbox, "sandbox", false, "Run the agent inside a Docker sandbox (requires Docker Desktop with sandbox support)")
	cmd.PersistentFlags().StringVar(&flags.sandboxTemplate, "template", "docker/sandbox-templates:docker-agent", "Template image for the sandbox (passed to docker sandbox create -t)")
	cmd.PersistentFlags().BoolVar(&flags.sbx, "sbx", true, "Prefer the sbx CLI backend when available (set --sbx=false to force docker sandbox)")
	cmd.PersistentFlags().BoolVar(&flags.noKit, "no-kit", false, "Do not stage a docker-agent kit (skills, prompt files) when running in a sandbox")
	cmd.PersistentFlags().StringVar(&flags.agentPickerSpec, "agent-picker", "", "Show a full-screen picker to choose an agent before launching. Optional comma-separated list of agent refs (defaults to \"default,coder\")")
	cmd.PersistentFlags().Lookup("agent-picker").NoOptDefVal = strings.Join(defaultAgentPickerRefs, ",")
	cmd.PersistentFlags().StringVarP(&flags.worktreeName, "worktree", "w", "", "Run the agent in a fresh git worktree of the working directory (isolates changes from your checkout). Optionally name it: --worktree=my-name")
	cmd.PersistentFlags().Lookup("worktree").NoOptDefVal = worktreeAutoName
	cmd.PersistentFlags().StringVar(&flags.worktreePR, "worktree-pr", "", "Run the agent in a git worktree checked out on an existing GitHub pull request (number or URL). Continues the PR's branch; requires the GitHub CLI (gh).")
	cmd.PersistentFlags().StringVar(&flags.worktreeBase, "worktree-base", "", "Branch the --worktree from this ref instead of the current HEAD (e.g. main, origin/main). A remote-tracking ref is fetched first so the worktree starts from the latest remote state.")
	cmd.PersistentFlags().BoolVar(&flags.sessionReadOnly, "session-read-only", false, "Open the session in read-only mode (view conversation history but prevent new messages)")
	cmd.MarkFlagsMutuallyExclusive("fake", "record")
	cmd.MarkFlagsMutuallyExclusive("remote", "sandbox")
	cmd.MarkFlagsMutuallyExclusive("remote", "session-db")
	cmd.MarkFlagsMutuallyExclusive("remote", "session")
	cmd.MarkFlagsMutuallyExclusive("remote", "record")
	cmd.MarkFlagsMutuallyExclusive("remote", "fake")
	// A worktree is a local directory: it has no meaning for a remote runtime
	// and is not wired through the sandbox boundary.
	cmd.MarkFlagsMutuallyExclusive("remote", "worktree")
	cmd.MarkFlagsMutuallyExclusive("sandbox", "worktree")
	cmd.MarkFlagsMutuallyExclusive("remote", "worktree-pr")
	cmd.MarkFlagsMutuallyExclusive("sandbox", "worktree-pr")
	cmd.MarkFlagsMutuallyExclusive("worktree", "worktree-pr")
	// --worktree-base picks the start-point of the branch --worktree creates,
	// so it is meaningless for a PR worktree (which continues the PR's branch)
	// or a remote/sandbox run (which has no local worktree).
	cmd.MarkFlagsMutuallyExclusive("worktree-base", "worktree-pr")
	cmd.MarkFlagsMutuallyExclusive("remote", "worktree-base")
	cmd.MarkFlagsMutuallyExclusive("sandbox", "worktree-base")

	// --exec only
	cmd.PersistentFlags().BoolVar(&flags.exec, "exec", false, "Execute without a TUI")
	cmd.PersistentFlags().BoolVar(&flags.hideToolCalls, "hide-tool-calls", false, "Hide the tool calls in the output")
	cmd.PersistentFlags().BoolVar(&flags.outputJSON, "json", false, "Output results in JSON format")
}

func (f *runExecFlags) runRunCommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()

	if f.exec {
		telemetry.TrackCommand(ctx, "exec", args)
		defer func() { // do not inline this defer so that commandErr is not resolved early
			telemetry.TrackCommandError(ctx, "exec", args, commandErr)
		}()
	} else {
		telemetry.TrackCommand(ctx, "run", args)
		defer func() { // do not inline this defer so that commandErr is not resolved early
			telemetry.TrackCommandError(ctx, "run", args, commandErr)
		}()
	}

	// Validate an explicit --theme value early so a typo fails fast with a
	// helpful message instead of silently falling back to the default theme
	// once the TUI starts.
	if f.theme != "" {
		if err := validateTheme(f.theme); err != nil {
			return err
		}
	}

	useTUI := !f.exec && (f.forceTUI || isatty.IsTerminal(os.Stdout.Fd()))

	// When --agent-picker is set, show a full-screen picker up front and use
	// the chosen ref as the agent to run. Resolving it here (before sandbox
	// and alias resolution) means the selected agent's own sandbox/alias
	// defaults are honoured exactly as if it had been passed positionally.
	// The picker is interactive, so it requires a TUI.
	if cmd.Flags().Changed("agent-picker") {
		if !useTUI {
			return errors.New("--agent-picker requires an interactive terminal and cannot be used with --exec")
		}
		refs := parseAgentPickerRefs(f.agentPickerSpec)
		applyTheme(f.theme)
		chosen, err := selectAgentRef(ctx, refs, f.runConfig.EnvProvider())
		if err != nil {
			if errors.Is(err, errAgentPickerCancelled) {
				cli.NewPrinter(cmd.OutOrStdout()).Println("Agent selection cancelled.")
				return nil
			}
			return err
		}
		// With --agent-picker the agent comes from the picker, so any
		// positional args are messages. Prepend the chosen ref so the rest
		// of the pipeline (which expects args[0] to be the agent) is happy.
		args = prependAgentRef(chosen, args)
	}

	// Resolve alias / runtime-declared sandbox opt-in before dispatch.
	// An explicit --sandbox=<bool> on the CLI always wins, so we only
	// consult the lower-priority sources when the flag wasn't set.
	var agentCfg *latestcfg.Config
	if !cmd.Flags().Changed("sandbox") {
		var agentRef string
		if len(args) > 0 {
			agentRef = args[0]
		}
		f.sandbox, agentCfg = resolveSandboxDefault(ctx, agentRef, f.sandbox)
	}

	if f.sandbox {
		if cmd.Flags().Changed("worktree") || cmd.Flags().Changed("worktree-pr") {
			return errors.New("--worktree/--worktree-pr cannot be combined with a sandboxed run")
		}
		return runInSandbox(ctx, cmd, args, &f.runConfig, f.sandboxTemplate, f.sbx, f.noKit, agentCfg)
	}

	// --worktree was provided (with or without a value). The string flag lets
	// users name the worktree (--worktree=my-name); without a value cobra
	// stores the sentinel that triggers a random name.
	f.worktree = cmd.Flags().Changed("worktree")

	// --worktree-base only selects the start-point of the branch --worktree
	// creates; on its own it would silently do nothing, so reject it.
	if f.worktreeBase != "" && !f.worktree {
		return errors.New("--worktree-base requires --worktree")
	}
	f.leanChanged = cmd.Flags().Changed("lean")

	out := cli.NewPrinter(cmd.OutOrStdout())

	return f.runOrExec(ctx, out, args, useTUI)
}

func (f *runExecFlags) runOrExec(ctx context.Context, out *cli.Printer, args []string, useTUI bool) error {
	slog.DebugContext(ctx, "Starting agent", "agent", f.agentName)

	// Start profiling if requested
	stopProfiling, err := profiling.Start(f.cpuProfile, f.memProfile)
	if err != nil {
		return err
	}
	defer func() {
		if err := stopProfiling(); err != nil {
			slog.ErrorContext(ctx, "Profiling cleanup failed", "error", err)
		}
	}()

	var agentFileName string
	if len(args) > 0 {
		agentFileName = args[0]
	}

	// Apply global user settings first (lowest priority)
	// User settings only apply if the flag wasn't explicitly set by the user
	userSettings := userconfig.Get()
	f.applyUserSettings(ctx, userSettings)

	// Apply alias options if this is an alias reference
	// Alias options only apply if the flag wasn't explicitly set by the user
	if alias := config.ResolveAlias(agentFileName); alias != nil {
		slog.DebugContext(ctx, "Applying alias options", "yolo", alias.Yolo, "model", alias.Model, "hide_tool_results", alias.HideToolResults, "sandbox", alias.Sandbox)
		if alias.Yolo && !f.autoApprove {
			f.autoApprove = true
		}
		if alias.Model != "" && len(f.modelOverrides) == 0 {
			f.modelOverrides = append(f.modelOverrides, alias.Model)
		}
		if alias.HideToolResults && !f.hideToolResults {
			f.hideToolResults = true
		}
		// alias.Sandbox is consumed earlier in runRunCommand before
		// dispatch; reaching runOrExec means the sandbox decision
		// resolved to false (or the user opted out via --sandbox=false),
		// so flipping it here would be a no-op.
	}

	// Build global permissions checker from user config settings.
	if userSettings.Permissions != nil {
		f.globalPermissions = permissions.NewChecker(userSettings.Permissions)
	}

	// Start fake proxy if --fake is specified
	fakeCleanup, err := setupFakeProxy(f.fakeResponses, f.fakeStreamDelay, &f.runConfig)
	if err != nil {
		return err
	}
	defer func() {
		if err := fakeCleanup(); err != nil {
			slog.ErrorContext(ctx, "Failed to cleanup fake proxy", "error", err)
		}
	}()

	// Record AI API interactions to a cassette file if --record flag is specified.
	cassettePath, recordCleanup, err := setupRecordingProxy(f.recordPath, &f.runConfig)
	if err != nil {
		return err
	}
	if cassettePath != "" {
		defer func() {
			if err := recordCleanup(); err != nil {
				slog.ErrorContext(ctx, "Failed to cleanup recording proxy", "error", err)
			}
		}()
		out.Println("Recording mode enabled, cassette: " + cassettePath)
	}

	b, err := f.selectBackend(agentFileName)
	if err != nil {
		return err
	}
	defer func() {
		if err := b.Close(); err != nil {
			slog.ErrorContext(ctx, "Failed to close backend", "error", err)
		}
	}()

	if f.dryRun {
		// A dry run initializes the team but runs nothing, so it never
		// creates a worktree.
		loadResult, err := b.LoadTeam(ctx, b.LoadTeamRequest())
		if err != nil {
			return err
		}
		if loadResult != nil {
			stopToolSets(loadResult.Team)
		}
		out.Println("Dry run mode enabled. Agent initialized but will not execute.")
		return nil
	}

	// Create the worktree BEFORE loading the team. Toolsets capture the
	// working directory when they are built, so the worktree must already be
	// the working directory by then for every tool — the shell included — to
	// operate inside it rather than the user's checkout.
	//
	// The base directory is the process working directory, which already
	// reflects --working-dir: addGatewayFlags' PersistentPreRunE chdirs there
	// before the run. That is what lets --worktree and --working-dir compose —
	// --working-dir selects the repository the worktree is branched from.
	baseDir, _ := os.Getwd()

	// Resuming a session that ran in a worktree: reattach to that worktree's
	// directory so the shell and every other tool operate inside it again,
	// without the caller re-passing --worktree (which would fail because the
	// worktree already exists). An explicit --worktree/--worktree-pr on this
	// run takes precedence and creates a new one as usual.
	if !f.worktree && f.worktreePR == "" {
		if resumeDir, ok := b.ResumeWorkingDir(ctx); ok {
			baseDir = resumeDir
			f.runConfig.WorkingDir = resumeDir
		}
	}

	loadResult, createdWorktree, wd, err := f.loadTeamInWorktree(ctx, b, baseDir)
	if err != nil {
		return err
	}
	if createdWorktree != nil {
		out.Println("Using git worktree: " + createdWorktree.Dir + " (branch " + createdWorktree.Branch + ")")
		// loadResult is nil for the remote backend; worktrees are mutually
		// exclusive with --remote so this is belt-and-suspenders, matching
		// the nil-guard used for cleanup throughout this function.
		if loadResult != nil {
			if err := f.dispatchWorktreeCreate(ctx, out, loadResult.Team, createdWorktree); err != nil {
				stopToolSets(loadResult.Team)
				return err
			}
		}
	}
	rt, sess, cleanup, err := b.CreateSession(ctx, loadResult, b.CreateSessionRequest(wd))
	if err != nil {
		return err
	}
	defer cleanup()

	if !useTUI {
		// Non-interactive (--exec) runs never clean up the worktree: there
		// is no safe moment to prompt, and silently discarding work would
		// be surprising. The worktree is left in place for later inspection.
		return f.handleExecMode(ctx, out, rt, sess, args)
	}

	listenOpt, err := f.startAttachedServer(ctx, out, rt, sess)
	if err != nil {
		return err
	}

	applyTheme(f.theme)
	opts, err := f.buildAppOpts(args)
	if err != nil {
		return err
	}
	if listenOpt != nil {
		opts = append(opts, listenOpt)
	}

	eventHooks, err := parseOnEventFlags(f.onEventSpecs)
	if err != nil {
		return err
	}
	if hookOpt := withEventHooks(eventHooks); hookOpt != nil {
		opts = append(opts, hookOpt)
	}

	runErr := func() error {
		if f.lean {
			return f.runLeanTUI(ctx, rt, sess, cleanup, args, opts...)
		}
		return runTUI(ctx, rt, sess, b.Spawner(rt), cleanup, f.tuiOpts(), opts...)
	}()
	if runErr != nil {
		// On a TUI error we deliberately leave the worktree in place rather
		// than risk discarding work after an abnormal exit.
		return runErr
	}

	// The interactive session is over. Offer to clean up the worktree we
	// created for it (never a pre-existing one, since Create only makes new
	// worktrees). Shut the session down first so tools release any file
	// handles inside the worktree before we try to remove it; cleanup is
	// idempotent, so the deferred call above becomes a no-op.
	//
	// A fresh context is used because the TUI may have exited via a
	// canceled ctx (Ctrl-C), which would otherwise abort the prompt and
	// the git commands.
	if createdWorktree != nil {
		cleanup()
		f.cleanupWorktree(context.WithoutCancel(ctx), out, createdWorktree)
	}
	return nil
}

// loadTeamInWorktree creates the requested worktree (if any) and then loads
// the team with the worktree as its working directory. The ordering matters:
// toolsets capture runConfig.WorkingDir when they are constructed during
// LoadTeam, so the worktree must be settled first for every tool — the shell
// included — to operate inside the worktree instead of the user's checkout.
//
// It returns the loaded team, the created worktree (nil when neither
// --worktree nor --worktree-pr was given) and the working directory to run in
// (the worktree's directory when one was created, otherwise wd unchanged).
func (f *runExecFlags) applyUserSettings(ctx context.Context, userSettings *userconfig.Settings) {
	if userSettings.HideToolResults && !f.hideToolResults {
		f.hideToolResults = true
		slog.DebugContext(ctx, "Applying user settings", "hide_tool_results", true)
	}
	if userSettings.YOLO && !f.autoApprove {
		f.autoApprove = true
		slog.DebugContext(ctx, "Applying user settings", "YOLO", true)
	}
	if userSettings.Lean && !f.leanChanged && !f.lean {
		f.lean = true
		slog.DebugContext(ctx, "Applying user settings", "lean", true)
	}
	if userSettings.SnapshotsEnabled() {
		f.snapshotsEnabled = true
		slog.DebugContext(ctx, "Applying user settings", "snapshot", true)
	}
}

func (f *runExecFlags) loadTeamInWorktree(ctx context.Context, b backend, wd string) (*teamloader.LoadResult, *worktree.Worktree, string, error) {
	createdWorktree, err := f.setupWorktree(ctx, wd)
	if err != nil {
		return nil, nil, wd, err
	}
	if createdWorktree != nil {
		wd = createdWorktree.Dir
		f.runConfig.WorkingDir = createdWorktree.Dir
	}

	loadResult, err := b.LoadTeam(ctx, b.LoadTeamRequest())
	if err != nil {
		// The worktree was created before the load; tear it down so a load
		// failure doesn't leave an orphaned worktree behind. A fresh context
		// is used because the load may have failed on a cancelled ctx (Ctrl-C),
		// which would otherwise kill the git removal subprocess and orphan the
		// worktree.
		if createdWorktree != nil {
			if rmErr := createdWorktree.Remove(context.WithoutCancel(ctx)); rmErr != nil {
				slog.WarnContext(ctx, "Failed to remove worktree after load error", "dir", createdWorktree.Dir, "error", rmErr)
			}
		}
		return nil, nil, wd, err
	}
	return loadResult, createdWorktree, wd, nil
}

// setupWorktree creates the git worktree requested by --worktree or
// --worktree-pr, returning nil when neither was given. The returned worktree
// (when non-nil) becomes the session's working directory and is cleaned up
// when an interactive run ends.
func (f *runExecFlags) setupWorktree(ctx context.Context, wd string) (*worktree.Worktree, error) {
	switch {
	case f.worktreePR != "":
		wt, err := worktree.CreatePR(ctx, wd, f.worktreePR)
		if err != nil {
			switch {
			case errors.Is(err, worktree.ErrNotGitRepository):
				return nil, fmt.Errorf("--worktree-pr requires %s to be inside a git repository", wd)
			case errors.Is(err, worktree.ErrInvalidPRRef):
				return nil, fmt.Errorf("invalid --worktree-pr value: %w", err)
			case errors.Is(err, worktree.ErrGHNotFound):
				return nil, fmt.Errorf("--worktree-pr requires the GitHub CLI: %w", err)
			default:
				return nil, err
			}
		}
		return wt, nil

	case f.worktree:
		name := f.worktreeName
		if name == worktreeAutoName {
			name = ""
		}
		wt, err := worktree.Create(ctx, wd, name, worktree.WithBase(f.worktreeBase))
		if err != nil {
			switch {
			case errors.Is(err, worktree.ErrNotGitRepository):
				return nil, fmt.Errorf("--worktree requires %s to be inside a git repository", wd)
			case errors.Is(err, worktree.ErrInvalidName):
				return nil, fmt.Errorf("invalid --worktree name: %w", err)
			case errors.Is(err, worktree.ErrInvalidBase):
				return nil, fmt.Errorf("invalid --worktree-base: %w", err)
			default:
				return nil, err
			}
		}
		return wt, nil

	default:
		return nil, nil
	}
}

// cleanupWorktree removes a worktree created for an interactive run once it
// ends. A clean worktree (no uncommitted changes, untracked files, or new
// commits) is removed automatically. A dirty one is kept unless the user
// explicitly asks to remove it, so work is never discarded silently.
// Failures are reported but never abort the command — the run already
// succeeded.
func (f *runExecFlags) cleanupWorktree(ctx context.Context, out *cli.Printer, wt *worktree.Worktree) {
	st, err := wt.Status(ctx)
	if err != nil {
		out.Println("Could not inspect git worktree " + wt.Dir + ": " + err.Error())
		out.Println("Leaving it in place. Remove it manually with: git -C " + wt.SourceDir + " worktree remove " + wt.Dir)
		return
	}

	if st.IsDirty() {
		if !promptRemoveDirtyWorktree(ctx, out, wt, st) {
			out.Println("Keeping git worktree " + wt.Dir + " (branch " + wt.Branch + ").")
			return
		}
	}

	if err := wt.Remove(ctx); err != nil {
		out.Println("Failed to remove git worktree " + wt.Dir + ": " + err.Error())
		return
	}
	out.Println("Removed git worktree " + wt.Dir + " (branch " + wt.Branch + ").")
}

// promptRemoveDirtyWorktree asks the user whether to discard a worktree that
// still holds work. It defaults to keeping (returns false) on any non-yes
// answer or read error, so uncommitted work is never lost by accident.
func promptRemoveDirtyWorktree(ctx context.Context, out *cli.Printer, wt *worktree.Worktree, st worktree.Status) bool {
	var held []string
	if st.Modified {
		held = append(held, "uncommitted changes")
	}
	if st.Untracked {
		held = append(held, "untracked files")
	}
	if st.NewCommits {
		held = append(held, "new commits")
	}

	out.Println("\nThe git worktree " + wt.Dir + " (branch " + wt.Branch + ") still has " + strings.Join(held, ", ") + ".")
	out.Println("Remove it and discard this work? Keeping preserves the directory and branch so you can return later. (y/N):")

	response, err := input.ReadLine(ctx, os.Stdin)
	if err != nil {
		return false
	}
	response = strings.TrimSpace(strings.ToLower(response))
	return response == "y" || response == "yes"
}

// dispatchWorktreeCreate fires the worktree_create hooks of the agent the
// run targets, just after the worktree is created and before the session
// exists. Unlike every other event, this is dispatched from the CLI rather
// than the run loop: the worktree (and the working directory the runtime,
// session, tools and snapshot machinery all capture) must be settled first.
// Hooks run inside the new worktree so setup commands (copy .env, install
// deps) operate on the fresh checkout. A blocking verdict aborts the run.
func (f *runExecFlags) dispatchWorktreeCreate(ctx context.Context, out *cli.Printer, t *team.Team, wt *worktree.Worktree) error {
	agt, err := t.AgentOrDefault(f.agentName)
	if err != nil {
		return err
	}
	hooksCfg := agt.Hooks()
	if hooksCfg == nil {
		return nil
	}

	executor := hooks.NewExecutor(hooksCfg, wt.Dir, os.Environ())
	if !executor.Has(hooks.EventWorktreeCreate) {
		return nil
	}

	result, err := executor.Dispatch(ctx, hooks.EventWorktreeCreate, &hooks.Input{
		AgentName:         agt.Name(),
		Cwd:               wt.Dir,
		WorktreePath:      wt.Dir,
		WorktreeBranch:    wt.Branch,
		WorktreeSourceDir: wt.SourceDir,
	})
	if err != nil {
		return fmt.Errorf("running worktree_create hooks: %w", err)
	}
	if result.SystemMessage != "" {
		out.Println(result.SystemMessage)
	}
	if result.AdditionalContext != "" {
		out.Println(result.AdditionalContext)
	}
	if !result.Allowed {
		msg := result.Message
		if msg == "" {
			msg = "a worktree_create hook blocked the run"
		}
		return fmt.Errorf("worktree_create hook aborted the run: %s", msg)
	}
	return nil
}

func (f *runExecFlags) loadAgentFrom(ctx context.Context, req runtime.LoadTeamRequest) (*teamloader.LoadResult, error) {
	opts := append(loaderdefaults.Opts(), teamloader.WithModelOverrides(req.ModelOverrides))
	if len(req.PromptFiles) > 0 {
		opts = append(opts, teamloader.WithPromptFiles(req.PromptFiles))
	}
	return teamloader.LoadWithConfig(ctx, req.Source, req.RunConfig, opts...)
}

// runtimeOpts returns the runtime options derived from the current flags,
// the loaded team and the runtime configuration. The session store and the
// current agent name are passed in because they're resolved by callers from
// different sources (e.g. the spawner uses the same store as the parent).
func (f *runExecFlags) runtimeOpts(loadResult *teamloader.LoadResult, runConfig *config.RuntimeConfig, sessStore session.Store, agentName string) []runtime.Opt {
	modelSwitcherCfg := &runtime.ModelSwitcherConfig{
		Models:             loadResult.Models,
		Providers:          loadResult.Providers,
		ModelsGateway:      runConfig.ModelsGateway,
		EnvProvider:        runConfig.EnvProvider(),
		ProviderRegistry:   loadResult.ProviderRegistry,
		AgentDefaultModels: loadResult.AgentDefaultModels,
	}
	opts := []runtime.Opt{
		runtime.WithSessionStore(sessStore),
		runtime.WithCurrentAgent(agentName),
		runtime.WithWorkingDir(runConfig.WorkingDir),
		runtime.WithTracer(otel.Tracer(AppName)),
		runtime.WithModelSwitcherConfig(modelSwitcherCfg),
	}
	return opts
}

// snapshotRuntimeOpts wires the snapshot builtin into a runtime.
// Returns the [runtime.Opt]s that hand the registry and the
// [builtins.SnapshotController] auto-injector to the runtime, plus
// the controller itself for the embedder to pass to the App via
// [app.WithSnapshotController]. When snapshots aren't enabled,
// returns no opts and a nil controller so callers don't have to
// branch on f.snapshotsEnabled themselves.
//
// A fresh registry is created here rather than reused across runtimes
// so the spawner-created sub-runtimes get their own snapshot state
// (each spawned session has independent /undo history).
func (f *runExecFlags) snapshotRuntimeOpts() ([]runtime.Opt, builtins.SnapshotController, error) {
	if !f.snapshotsEnabled {
		return nil, nil, nil
	}
	reg := hooks.NewRegistry()
	ctrl, err := builtins.RegisterSnapshot(reg, true)
	if err != nil {
		return nil, nil, fmt.Errorf("register snapshot builtin: %w", err)
	}
	return []runtime.Opt{
		runtime.WithHooksRegistry(reg),
		runtime.WithAutoInjector(ctrl),
	}, ctrl, nil
}

func (f *runExecFlags) createLocalRuntimeAndSession(ctx context.Context, loadResult *teamloader.LoadResult, req runtime.CreateSessionRequest, sessStore session.Store) (runtime.Runtime, *session.Session, error) {
	t := loadResult.Team

	// Merge user-level global permissions into the team's checker so the
	// runtime receives a single, already-merged permission set.
	if req.GlobalPermissions != nil && !req.GlobalPermissions.IsEmpty() {
		t.SetPermissions(permissions.Merge(t.Permissions(), req.GlobalPermissions))
	}

	agt, err := t.AgentOrDefault(req.AgentName)
	if err != nil {
		return nil, nil, err
	}
	agentName := agt.Name()

	rtOpts, ctrl, err := f.snapshotRuntimeOpts()
	if err != nil {
		return nil, nil, err
	}
	runtimeOpts := append(f.runtimeOpts(loadResult, &f.runConfig, sessStore, agentName), rtOpts...)
	localRt, err := runtime.New(t, runtimeOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("creating runtime: %w", err)
	}
	f.snapshotController = ctrl

	var sess *session.Session
	if req.ResumeSessionID != "" {
		// Resolve relative session references (e.g., "-1" for last session)
		resolvedID, err := session.ResolveSessionID(ctx, sessStore, req.ResumeSessionID)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving session %q: %w", req.ResumeSessionID, err)
		}

		// Load existing session
		sess, err = sessStore.GetSession(ctx, resolvedID)
		switch {
		case err == nil:
			sess.ToolsApproved = req.ToolsApproved
			sess.HideToolResults = req.HideToolResults

			// Apply any stored model overrides from the session
			if len(sess.AgentModelOverrides) > 0 && localRt.SupportsModelSwitching() {
				for agentName, modelRef := range sess.AgentModelOverrides {
					if err := localRt.SetAgentModel(ctx, agentName, modelRef); err != nil {
						slog.WarnContext(ctx, "Failed to apply stored model override", "agent", agentName, "model", modelRef, "error", err)
					}
				}
			}

			slog.DebugContext(ctx, "Loaded existing session", "session_id", resolvedID, "session_ref", req.ResumeSessionID, "agent", agentName)
		case errors.Is(err, session.ErrNotFound) && !session.IsRelativeSessionRef(req.ResumeSessionID):
			// An explicit, caller-chosen ID that doesn't exist yet: create the
			// session with that ID rather than failing. This lets a supervisor
			// (e.g. a board reconnecting to a dead agent) own the ID up front and
			// reuse it across runs — the first run creates, later runs resume.
			// A relative ref (-1, -2, ...) never lands here: it must resolve
			// against existing sessions.
			sess = session.New(append(f.buildSessionOpts(agt, req), session.WithID(resolvedID))...)
			slog.DebugContext(ctx, "Creating session with caller-supplied ID", "session_id", resolvedID, "agent", agentName)
		default:
			return nil, nil, fmt.Errorf("loading session %q: %w", resolvedID, err)
		}
	} else {
		sess = session.New(f.buildSessionOpts(agt, req)...)
		// Session is stored lazily on first UpdateSession call (when content is added)
		// This avoids creating empty sessions in the database
		slog.DebugContext(ctx, "Using local runtime", "agent", agentName)
	}

	return localRt, sess, nil
}

func (f *runExecFlags) handleExecMode(ctx context.Context, out *cli.Printer, rt runtime.Runtime, sess *session.Session, args []string) error {
	if f.sessionReadOnly {
		return errors.New("--session-read-only cannot be used with --exec: there is nothing to display without a TUI")
	}

	// args[0] is the agent file; args[1:] are user messages for multi-turn conversation
	var userMessages []string
	if len(args) > 1 {
		userMessages = args[1:]
	}

	err := cli.Run(ctx, out, cli.Config{
		AppName:        AppName,
		AttachmentPath: f.attachmentPath,
		HideToolCalls:  f.hideToolCalls,
		OutputJSON:     f.outputJSON,
		AutoApprove:    f.autoApprove,
	}, rt, sess, userMessages)
	if cliErr, ok := errors.AsType[cli.RuntimeError](err); ok {
		return RuntimeError{Err: cliErr.Err}
	}
	return err
}

func readInitialMessage(args []string) (*string, error) {
	if len(args) < 2 {
		return nil, nil
	}

	if args[1] == "-" {
		buf, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("failed to read from stdin: %w", err)
		}
		text := string(buf)
		return &text, nil
	}

	return &args[1], nil
}

// tuiOpts returns the TUI options derived from the current flags.
func (f *runExecFlags) tuiOpts() []tui.Option {
	var opts []tui.Option
	if f.lean {
		opts = append(opts, tui.WithLeanMode())
	}
	if f.appName != "" {
		opts = append(opts, tui.WithAppName(f.appName))
	}
	if len(f.disabledCommands) > 0 {
		opts = append(opts, tui.WithDisabledCommands(f.disabledCommands))
	}
	if !f.sidebar {
		opts = append(opts, tui.WithHideSidebar())
	}
	return opts
}

// runLeanTUI builds the App and drives the standalone lean TUI, used when
// --lean is set. Unlike the full TUI it renders to the normal terminal buffer
// (no alternate screen) and sends the first/queued messages itself rather than
// through the App's bubbletea command pipeline.
func (f *runExecFlags) runLeanTUI(ctx context.Context, rt runtime.Runtime, sess *session.Session, cleanup func(), args []string, opts ...app.Opt) error {
	if gen := rt.TitleGenerator(); gen != nil {
		opts = append(opts, app.WithTitleGenerator(gen))
	}
	a := app.New(ctx, rt, sess, opts...)

	firstMessage, err := readInitialMessage(args)
	if err != nil {
		return err
	}
	var queued []string
	if len(args) > 2 {
		queued = args[2:]
	}
	if cleanup == nil {
		cleanup = func() {}
	}

	wd, _ := os.Getwd()
	return leantui.Run(ctx, leantui.Config{
		App:                    a,
		WorkingDir:             wd,
		Cleanup:                cleanup,
		FirstMessage:           firstMessage,
		FirstMessageAttachment: f.attachmentPath,
		QueuedMessages:         queued,
		AppName:                f.appName,
		DisabledCommands:       f.disabledCommands,
	})
}

func (f *runExecFlags) buildAppOpts(args []string) ([]app.Opt, error) {
	firstMessage, err := readInitialMessage(args)
	if err != nil {
		return nil, err
	}

	var opts []app.Opt
	if firstMessage != nil {
		opts = append(opts, app.WithFirstMessage(*firstMessage))
	} else if f.attachmentPath != "" {
		// When --attach is used without an explicit message, provide a default
		// so that SendFirstMessage processes the attachment.
		defaultMsg := ""
		opts = append(opts, app.WithFirstMessage(defaultMsg))
	}
	if len(args) > 2 {
		opts = append(opts, app.WithQueuedMessages(args[2:]))
	}
	if f.attachmentPath != "" {
		opts = append(opts, app.WithFirstMessageAttachment(f.attachmentPath))
	}
	if f.exitAfterResponse {
		opts = append(opts, app.WithExitAfterFirstResponse())
	}
	if f.snapshotController != nil {
		opts = append(opts, app.WithSnapshotController(f.snapshotController))
	}
	if f.sessionReadOnly {
		opts = append(opts, app.WithReadOnly())
	}
	return opts, nil
}

// buildSessionOpts returns the canonical set of session options derived from
// CLI flags and agent configuration. Both the initial session and spawned
// sessions use this method so their options never drift apart.
func (f *runExecFlags) buildSessionOpts(agt *agent.Agent, req runtime.CreateSessionRequest) []session.Opt {
	return []session.Opt{
		session.WithMaxIterations(agt.MaxIterations()),
		session.WithMaxConsecutiveToolCalls(agt.MaxConsecutiveToolCalls()),
		session.WithMaxOldToolCallTokens(agt.MaxOldToolCallTokens()),
		session.WithToolsApproved(req.ToolsApproved),
		session.WithHideToolResults(req.HideToolResults),
		session.WithWorkingDir(req.WorkingDir),
	}
}

// createSessionSpawner creates a function that can spawn new sessions with different working directories.
func (f *runExecFlags) createSessionSpawner(agentSource config.Source, sessStore session.Store) tui.SessionSpawner {
	return func(spawnCtx context.Context, workingDir string) (*app.App, *session.Session, func(), error) {
		// Create a copy of the runtime config with the new working directory
		runConfigCopy := f.runConfig.Clone()
		runConfigCopy.WorkingDir = workingDir

		// Load team with the new working directory, honouring every flag the
		// initial load already honours (model overrides AND prompt files).
		loadReq := f.loadTeamRequest(agentSource)
		loadReq.RunConfig = runConfigCopy
		loadResult, err := f.loadAgentFrom(spawnCtx, loadReq)
		if err != nil {
			return nil, nil, nil, err
		}

		t := loadResult.Team
		agt, err := t.AgentOrDefault(f.agentName)
		if err != nil {
			return nil, nil, nil, err
		}

		// Merge global permissions into the team's checker
		if f.globalPermissions != nil && !f.globalPermissions.IsEmpty() {
			t.SetPermissions(permissions.Merge(t.Permissions(), f.globalPermissions))
		}

		rtOpts, ctrl, err := f.snapshotRuntimeOpts()
		if err != nil {
			return nil, nil, nil, err
		}
		runtimeOpts := append(f.runtimeOpts(loadResult, runConfigCopy, sessStore, agt.Name()), rtOpts...)
		localRt, err := runtime.New(t, runtimeOpts...)
		if err != nil {
			return nil, nil, nil, err
		}

		// Create a new session
		spawnReq := f.createSessionRequest(workingDir)
		spawnReq.AgentName = agt.Name()
		newSess := session.New(f.buildSessionOpts(agt, spawnReq)...)

		// Create cleanup function
		cleanup := func() {
			stopToolSets(t)
		}

		// Create the app
		var appOpts []app.Opt
		if gen := localRt.TitleGenerator(); gen != nil {
			appOpts = append(appOpts, app.WithTitleGenerator(gen))
		}
		if ctrl != nil {
			appOpts = append(appOpts, app.WithSnapshotController(ctrl))
		}

		a := app.New(spawnCtx, localRt, newSess, appOpts...)

		return a, newSess, cleanup, nil
	}
}

// toolStopper is the subset of *team.Team needed by stopToolSets.
type toolStopper interface {
	StopToolSets(ctx context.Context) error
}

// stopToolSets gracefully stops all tool sets with a bounded timeout so
// that cleanup cannot block indefinitely.
func stopToolSets(t toolStopper) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := t.StopToolSets(ctx); err != nil {
		slog.ErrorContext(ctx, "Failed to stop tool sets", "error", err)
	}
}

// validateTheme reports whether ref names a loadable theme. It is used to
// fail fast on an explicit --theme value, listing the available themes so the
// user can correct a typo.
func validateTheme(ref string) error {
	if _, err := styles.LoadTheme(ref); err != nil {
		if refs, listErr := styles.ListThemeRefs(); listErr == nil && len(refs) > 0 {
			return fmt.Errorf("unknown theme %q; available themes: %s", ref, strings.Join(refs, ", "))
		}
		return fmt.Errorf("unknown theme %q: %w", ref, err)
	}
	return nil
}

// applyTheme applies the theme, resolving it from the --theme flag, then the
// user config, then the built-in default.
func applyTheme(themeOverride string) {
	// Resolve theme from --theme flag > user config > built-in default
	themeRef := styles.DefaultThemeRef
	if userSettings := userconfig.Get(); userSettings.Theme != "" {
		themeRef = userSettings.Theme
	}
	if themeOverride != "" {
		themeRef = themeOverride
	}

	theme, err := styles.LoadTheme(themeRef)
	if err != nil {
		slog.Warn("Failed to load theme, using default", "theme", themeRef, "error", err)
		theme = styles.DefaultTheme()
	}

	styles.ApplyTheme(theme)
	slog.Debug("Applied theme", "theme_ref", themeRef, "theme_name", theme.Name)
}
