package root

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/profiling"
	"github.com/docker/docker-agent/pkg/reflectx"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tui"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
)

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

	// Exec only
	exec          bool
	hideToolCalls bool
	outputJSON    bool

	// Run only
	hideToolResults bool
	lean            bool
	listenAddr      string
	onEventSpecs    []string

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
	cmd.PersistentFlags().StringVar(&flags.sessionID, "session", "", "Continue from a previous session by ID or relative offset (e.g., -1 for last session)")
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
	cmd.PersistentFlags().BoolVar(&flags.sandbox, "sandbox", false, "Run the agent inside a Docker sandbox (requires Docker Desktop with sandbox support)")
	cmd.PersistentFlags().StringVar(&flags.sandboxTemplate, "template", "docker/sandbox-templates:docker-agent", "Template image for the sandbox (passed to docker sandbox create -t)")
	cmd.PersistentFlags().BoolVar(&flags.sbx, "sbx", true, "Prefer the sbx CLI backend when available (set --sbx=false to force docker sandbox)")
	cmd.PersistentFlags().BoolVar(&flags.noKit, "no-kit", false, "Do not stage a docker-agent kit (skills, prompt files) when running in a sandbox")
	cmd.MarkFlagsMutuallyExclusive("fake", "record")
	cmd.MarkFlagsMutuallyExclusive("remote", "sandbox")
	cmd.MarkFlagsMutuallyExclusive("remote", "session-db")
	cmd.MarkFlagsMutuallyExclusive("remote", "session")
	cmd.MarkFlagsMutuallyExclusive("remote", "record")
	cmd.MarkFlagsMutuallyExclusive("remote", "fake")

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

	if f.sandbox {
		return runInSandbox(ctx, cmd, args, &f.runConfig, f.sandboxTemplate, f.sbx, f.noKit)
	}

	out := cli.NewPrinter(cmd.OutOrStdout())

	useTUI := !f.exec && (f.forceTUI || isatty.IsTerminal(os.Stdout.Fd()))
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
	if userSettings.HideToolResults && !f.hideToolResults {
		f.hideToolResults = true
		slog.DebugContext(ctx, "Applying user settings", "hide_tool_results", true)
	}
	if userSettings.YOLO && !f.autoApprove {
		f.autoApprove = true
		slog.DebugContext(ctx, "Applying user settings", "YOLO", true)
	}
	if userSettings.SnapshotsEnabled() {
		f.snapshotsEnabled = true
		slog.DebugContext(ctx, "Applying user settings", "snapshot", true)
	}

	// Apply alias options if this is an alias reference
	// Alias options only apply if the flag wasn't explicitly set by the user
	if alias := config.ResolveAlias(agentFileName); alias != nil {
		slog.DebugContext(ctx, "Applying alias options", "yolo", alias.Yolo, "model", alias.Model, "hide_tool_results", alias.HideToolResults)
		if alias.Yolo && !f.autoApprove {
			f.autoApprove = true
		}
		if alias.Model != "" && len(f.modelOverrides) == 0 {
			f.modelOverrides = append(f.modelOverrides, alias.Model)
		}
		if alias.HideToolResults && !f.hideToolResults {
			f.hideToolResults = true
		}
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

	loadResult, err := b.LoadTeam(ctx, b.LoadTeamRequest())
	if err != nil {
		return err
	}

	if f.dryRun {
		if loadResult != nil {
			stopToolSets(loadResult.Team)
		}
		out.Println("Dry run mode enabled. Agent initialized but will not execute.")
		return nil
	}

	wd, _ := os.Getwd()
	rt, sess, cleanup, err := b.CreateSession(ctx, loadResult, b.CreateSessionRequest(wd))
	if err != nil {
		return err
	}
	defer cleanup()

	if !useTUI {
		return f.handleExecMode(ctx, out, rt, sess, args)
	}

	listenOpt, err := f.startAttachedServer(ctx, out, rt, sess)
	if err != nil {
		return err
	}

	applyTheme()
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

	return runTUI(ctx, rt, sess, b.Spawner(rt), cleanup, f.tuiOpts(), opts...)
}

func (f *runExecFlags) loadAgentFrom(ctx context.Context, req runtime.LoadTeamRequest) (*teamloader.LoadResult, error) {
	opts := []teamloader.Opt{
		teamloader.WithModelOverrides(req.ModelOverrides),
	}
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

func (f *runExecFlags) createLocalRuntimeAndSession(ctx context.Context, loadResult *teamloader.LoadResult, req runtime.CreateSessionRequest) (runtime.Runtime, *session.Session, error) {
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

	sessionDB, err := pathx.ExpandHomeDir(req.SessionDB)
	if err != nil {
		return nil, nil, err
	}

	sessStore, err := session.NewSQLiteSessionStore(sessionDB)
	if err != nil {
		return nil, nil, fmt.Errorf("creating session store: %w", err)
	}

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
		if err != nil {
			return nil, nil, fmt.Errorf("loading session %q: %w", resolvedID, err)
		}
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
	} else {
		sess = session.New(f.buildSessionOpts(agt, req)...)
		// Session is stored lazily on first UpdateSession call (when content is added)
		// This avoids creating empty sessions in the database
		slog.DebugContext(ctx, "Using local runtime", "agent", agentName)
	}

	return localRt, sess, nil
}

func (f *runExecFlags) handleExecMode(ctx context.Context, out *cli.Printer, rt runtime.Runtime, sess *session.Session, args []string) error {
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
	return opts
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
	if !reflectx.IsNil(f.snapshotController) {
		opts = append(opts, app.WithSnapshotController(f.snapshotController))
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
		if !reflectx.IsNil(ctrl) {
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

// applyTheme applies the theme from user config, or the built-in default.
func applyTheme() {
	// Resolve theme from user config > built-in default
	themeRef := styles.DefaultThemeRef
	if userSettings := userconfig.Get(); userSettings.Theme != "" {
		themeRef = userSettings.Theme
	}

	theme, err := styles.LoadTheme(themeRef)
	if err != nil {
		slog.Warn("Failed to load theme, using default", "theme", themeRef, "error", err)
		theme = styles.DefaultTheme()
	}

	styles.ApplyTheme(theme)
	slog.Debug("Applied theme", "theme_ref", themeRef, "theme_name", theme.Name)
}
