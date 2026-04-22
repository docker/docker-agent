package root

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/profiling"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
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

	// Exec only
	exec          bool
	hideToolCalls bool
	outputJSON    bool

	// Run only
	hideToolResults bool
	lean            bool
	showGitignored  bool

	// globalPermissions holds the user-level global permission checker built
	// from user config settings. Nil when no global permissions are configured.
	globalPermissions *permissions.Checker
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
	cmd.PersistentFlags().StringVarP(&flags.agentName, "agent", "a", "root", "Name of the agent to run")
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
	cmd.PersistentFlags().StringVar(&flags.cpuProfile, "cpuprofile", "", "Write CPU profile to file")
	_ = cmd.PersistentFlags().MarkHidden("cpuprofile")
	cmd.PersistentFlags().StringVar(&flags.memProfile, "memprofile", "", "Write memory profile to file")
	_ = cmd.PersistentFlags().MarkHidden("memprofile")
	cmd.PersistentFlags().BoolVar(&flags.forceTUI, "force-tui", false, "Force TUI mode even when not in a terminal")
	_ = cmd.PersistentFlags().MarkHidden("force-tui")
	cmd.PersistentFlags().BoolVar(&flags.lean, "lean", false, "Use a simplified TUI with minimal chrome")
	cmd.PersistentFlags().BoolVar(&flags.showGitignored, "show-gitignored", false, "Show gitignored files in the file picker")
	cmd.PersistentFlags().BoolVar(&flags.sandbox, "sandbox", false, "Run the agent inside a Docker sandbox (requires Docker Desktop with sandbox support)")
	cmd.PersistentFlags().StringVar(&flags.sandboxTemplate, "template", "docker/sandbox-templates:docker-agent", "Template image for the sandbox (passed to docker sandbox create -t)")
	cmd.PersistentFlags().BoolVar(&flags.sbx, "sbx", true, "Prefer the sbx CLI backend when available (set --sbx=false to force docker sandbox)")
	cmd.MarkFlagsMutuallyExclusive("fake", "record")

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
		return runInSandbox(ctx, cmd, args, &f.runConfig, f.sandboxTemplate, f.sbx)
	}

	out := cli.NewPrinter(cmd.OutOrStdout())

	useTUI := !f.exec && (f.forceTUI || isatty.IsTerminal(os.Stdout.Fd()))
	return f.runOrExec(ctx, out, args, useTUI)
}

func (f *runExecFlags) runOrExec(ctx context.Context, out *cli.Printer, args []string, useTUI bool) error {
	slog.Debug("Starting agent", "agent", f.agentName)

	// Start profiling if requested
	stopProfiling, err := profiling.Start(f.cpuProfile, f.memProfile)
	if err != nil {
		return err
	}
	defer func() {
		if err := stopProfiling(); err != nil {
			slog.Error("Profiling cleanup failed", "error", err)
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
		slog.Debug("Applying user settings", "hide_tool_results", true)
	}
	if userSettings.YOLO && !f.autoApprove {
		f.autoApprove = true
		slog.Debug("Applying user settings", "YOLO", true)
	}

	// Apply alias options if this is an alias reference
	// Alias options only apply if the flag wasn't explicitly set by the user
	if alias := config.ResolveAlias(agentFileName); alias != nil {
		slog.Debug("Applying alias options", "yolo", alias.Yolo, "model", alias.Model, "hide_tool_results", alias.HideToolResults)
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
			slog.Error("Failed to cleanup fake proxy", "error", err)
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
				slog.Error("Failed to cleanup recording proxy", "error", err)
			}
		}()
		out.Println("Recording mode enabled, cassette: " + cassettePath)
	}

	// Remote runtime
	if f.remoteAddress != "" {
		rt, sess, err := f.createRemoteRuntimeAndSession(ctx, agentFileName)
		if err != nil {
			return err
		}
		return f.launchTUI(ctx, out, rt, sess, args, useTUI)
	}

	// Local runtime
	agentSource, err := config.Resolve(agentFileName, f.runConfig.EnvProvider())
	if err != nil {
		return err
	}

	loadResult, err := f.loadAgentFrom(ctx, agentSource)
	if err != nil {
		return err
	}

	rt, sess, err := f.createLocalRuntimeAndSession(ctx, loadResult)
	if err != nil {
		return err
	}
	defer func() {
		if err := rt.Close(); err != nil {
			slog.Error("Failed to close runtime", "error", err)
		}
	}()
	var initialTeamCleanupOnce sync.Once
	initialTeamCleanup := func() {
		initialTeamCleanupOnce.Do(func() {
			stopToolSets(loadResult.Team)
		})
	}
	defer initialTeamCleanup()

	if f.dryRun {
		out.Println("Dry run mode enabled. Agent initialized but will not execute.")
		return nil
	}

	if !useTUI {
		return f.handleExecMode(ctx, out, rt, sess, args)
	}

	applyTheme()
	opts, err := f.buildAppOpts(args)
	if err != nil {
		return err
	}

	sessStore := rt.SessionStore()
	return runTUI(ctx, rt, sess, f.createSessionSpawner(agentSource, sessStore), initialTeamCleanup, f.tuiOpts(), opts...)
}

func (f *runExecFlags) loadAgentFrom(ctx context.Context, agentSource config.Source) (*teamloader.LoadResult, error) {
	opts := []teamloader.Opt{
		teamloader.WithModelOverrides(f.modelOverrides),
	}
	if len(f.promptFiles) > 0 {
		opts = append(opts, teamloader.WithPromptFiles(f.promptFiles))
	}
	return teamloader.LoadWithConfig(ctx, agentSource, &f.runConfig, opts...)
}

func (f *runExecFlags) createRemoteRuntimeAndSession(ctx context.Context, originalFilename string) (runtime.Runtime, *session.Session, error) {
	client, err := runtime.NewClient(f.remoteAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create remote client: %w", err)
	}

	sessTemplate := session.New(
		session.WithToolsApproved(f.autoApprove),
	)

	sess, err := client.CreateSession(ctx, sessTemplate)
	if err != nil {
		return nil, nil, err
	}

	remoteRt, err := runtime.NewRemoteRuntime(client,
		runtime.WithRemoteCurrentAgent(f.agentName),
		runtime.WithRemoteAgentFilename(originalFilename),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create remote runtime: %w", err)
	}

	slog.Debug("Using remote runtime", "address", f.remoteAddress, "agent", f.agentName)
	return remoteRt, sess, nil
}

func (f *runExecFlags) createLocalRuntimeAndSession(ctx context.Context, loadResult *teamloader.LoadResult) (runtime.Runtime, *session.Session, error) {
	t := loadResult.Team

	// Merge user-level global permissions into the team's checker so the
	// runtime receives a single, already-merged permission set.
	if f.globalPermissions != nil && !f.globalPermissions.IsEmpty() {
		t.SetPermissions(permissions.Merge(t.Permissions(), f.globalPermissions))
	}

	agt, err := t.Agent(f.agentName)
	if err != nil {
		return nil, nil, err
	}

	// Expand tilde in session database path
	sessionDB, err := expandTilde(f.sessionDB)
	if err != nil {
		return nil, nil, err
	}

	sessStore, err := session.NewSQLiteSessionStore(sessionDB)
	if err != nil {
		return nil, nil, fmt.Errorf("creating session store: %w", err)
	}

	// Create model switcher config for runtime model switching support
	modelSwitcherCfg := &runtime.ModelSwitcherConfig{
		Models:             loadResult.Models,
		Providers:          loadResult.Providers,
		ModelsGateway:      f.runConfig.ModelsGateway,
		EnvProvider:        f.runConfig.EnvProvider(),
		AgentDefaultModels: loadResult.AgentDefaultModels,
	}

	localRt, err := runtime.New(t,
		runtime.WithSessionStore(sessStore),
		runtime.WithCurrentAgent(f.agentName),
		runtime.WithTracer(otel.Tracer(AppName)),
		runtime.WithModelSwitcherConfig(modelSwitcherCfg),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("creating runtime: %w", err)
	}

	var sess *session.Session
	if f.sessionID != "" {
		// Resolve relative session references (e.g., "-1" for last session)
		resolvedID, err := session.ResolveSessionID(ctx, sessStore, f.sessionID)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving session %q: %w", f.sessionID, err)
		}

		// Load existing session
		sess, err = sessStore.GetSession(ctx, resolvedID)
		if err != nil {
			return nil, nil, fmt.Errorf("loading session %q: %w", resolvedID, err)
		}
		sess.ToolsApproved = f.autoApprove
		sess.HideToolResults = f.hideToolResults

		// Apply any stored model overrides from the session
		if len(sess.AgentModelOverrides) > 0 {
			if modelSwitcher, ok := localRt.(runtime.ModelSwitcher); ok {
				for agentName, modelRef := range sess.AgentModelOverrides {
					if err := modelSwitcher.SetAgentModel(ctx, agentName, modelRef); err != nil {
						slog.Warn("Failed to apply stored model override", "agent", agentName, "model", modelRef, "error", err)
					}
				}
			}
		}

		slog.Debug("Loaded existing session", "session_id", resolvedID, "session_ref", f.sessionID, "agent", f.agentName)
	} else {
		wd, _ := os.Getwd()
		sess = session.New(f.buildSessionOpts(agt, wd)...)
		// Session is stored lazily on first UpdateSession call (when content is added)
		// This avoids creating empty sessions in the database
		slog.Debug("Using local runtime", "agent", f.agentName)
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

func (f *runExecFlags) launchTUI(ctx context.Context, out *cli.Printer, rt runtime.Runtime, sess *session.Session, args []string, useTUI bool) error {
	if useTUI {
		applyTheme()
	}

	if f.dryRun {
		out.Println("Dry run mode enabled. Agent initialized but will not execute.")
		return nil
	}

	if !useTUI {
		return f.handleExecMode(ctx, out, rt, sess, args)
	}

	opts, err := f.buildAppOpts(args)
	if err != nil {
		return err
	}

	return runTUI(ctx, rt, sess, nil, nil, f.tuiOpts(), opts...)
}

// tuiOpts returns the TUI options derived from the current flags.
func (f *runExecFlags) tuiOpts() []tui.Option {
	var opts []tui.Option
	if f.lean {
		opts = append(opts, tui.WithLeanMode())
	}
	if f.showGitignored {
		opts = append(opts, tui.WithShowGitignored())
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
	return opts, nil
}

// buildSessionOpts returns the canonical set of session options derived from
// CLI flags and agent configuration. Both the initial session and spawned
// sessions use this method so their options never drift apart.
func (f *runExecFlags) buildSessionOpts(agt *agent.Agent, workingDir string) []session.Opt {
	return []session.Opt{
		session.WithMaxIterations(agt.MaxIterations()),
		session.WithMaxConsecutiveToolCalls(agt.MaxConsecutiveToolCalls()),
		session.WithMaxOldToolCallTokens(agt.MaxOldToolCallTokens()),
		session.WithToolsApproved(f.autoApprove),
		session.WithHideToolResults(f.hideToolResults),
		session.WithWorkingDir(workingDir),
	}
}

// createSessionSpawner creates a function that can spawn new sessions with different working directories.
func (f *runExecFlags) createSessionSpawner(agentSource config.Source, sessStore session.Store) tui.SessionSpawner {
	return func(spawnCtx context.Context, workingDir string) (*app.App, *session.Session, func(), error) {
		// Create a copy of the runtime config with the new working directory
		runConfigCopy := f.runConfig.Clone()
		runConfigCopy.WorkingDir = workingDir

		// Load team with the new working directory
		loadResult, err := teamloader.LoadWithConfig(spawnCtx, agentSource, runConfigCopy, teamloader.WithModelOverrides(f.modelOverrides))
		if err != nil {
			return nil, nil, nil, err
		}

		team := loadResult.Team
		agt, err := team.Agent(f.agentName)
		if err != nil {
			return nil, nil, nil, err
		}

		// Create model switcher config
		modelSwitcherCfg := &runtime.ModelSwitcherConfig{
			Models:             loadResult.Models,
			Providers:          loadResult.Providers,
			ModelsGateway:      runConfigCopy.ModelsGateway,
			EnvProvider:        runConfigCopy.EnvProvider(),
			AgentDefaultModels: loadResult.AgentDefaultModels,
		}

		// Merge global permissions into the team's checker
		if f.globalPermissions != nil && !f.globalPermissions.IsEmpty() {
			team.SetPermissions(permissions.Merge(team.Permissions(), f.globalPermissions))
		}

		// Create the local runtime
		localRt, err := runtime.New(team,
			runtime.WithSessionStore(sessStore),
			runtime.WithCurrentAgent(f.agentName),
			runtime.WithTracer(otel.Tracer(AppName)),
			runtime.WithModelSwitcherConfig(modelSwitcherCfg),
		)
		if err != nil {
			return nil, nil, nil, err
		}

		// Create a new session
		newSess := session.New(f.buildSessionOpts(agt, workingDir)...)

		// Create cleanup function
		cleanup := func() {
			stopToolSets(team)
		}

		// Create the app
		var appOpts []app.Opt
		if pr, ok := localRt.(*runtime.PersistentRuntime); ok {
			if model := pr.CurrentAgent().Model(); model != nil {
				appOpts = append(appOpts, app.WithTitleGenerator(sessiontitle.New(model)))
			}
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
		slog.Error("Failed to stop tool sets", "error", err)
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
