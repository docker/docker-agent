package root

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/paths"
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

	// Exec only
	exec          bool
	hideToolCalls bool
	outputJSON    bool

	// Run only
	hideToolResults bool
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
	cmd.PersistentFlags().BoolVar(&flags.sandbox, "sandbox", false, "Run the agent inside a Docker sandbox (requires Docker Desktop with sandbox support)")
	cmd.PersistentFlags().StringVar(&flags.sandboxTemplate, "template", "", "Template image for the sandbox (passed to docker sandbox create -t)")
	cmd.MarkFlagsMutuallyExclusive("fake", "record")

	// --exec only
	cmd.PersistentFlags().BoolVar(&flags.exec, "exec", false, "Execute without a TUI")
	cmd.PersistentFlags().BoolVar(&flags.hideToolCalls, "hide-tool-calls", false, "Hide the tool calls in the output")
	cmd.PersistentFlags().BoolVar(&flags.outputJSON, "json", false, "Output results in JSON format")
}

func (f *runExecFlags) runRunCommand(cmd *cobra.Command, args []string) error {
	// If --sandbox is set, delegate everything to docker sandbox.
	if f.sandbox {
		return runInSandbox(cmd, &f.runConfig, f.sandboxTemplate)
	}

	if f.exec {
		telemetry.TrackCommand("exec", args)
	} else {
		telemetry.TrackCommand("run", args)
	}

	ctx := cmd.Context()
	out := cli.NewPrinter(cmd.OutOrStdout())

	useTUI := !f.exec && (f.forceTUI || isatty.IsTerminal(os.Stdout.Fd()))
	return f.runOrExec(ctx, out, args, useTUI)
}

func (f *runExecFlags) runOrExec(ctx context.Context, out *cli.Printer, args []string, useTUI bool) (retErr error) {
	slog.Debug("Starting agent", "agent", f.agentName)

	// Start CPU profiling if requested
	if f.cpuProfile != "" {
		pf, err := os.Create(f.cpuProfile)
		if err != nil {
			return fmt.Errorf("failed to create CPU profile: %w", err)
		}
		if err := pprof.StartCPUProfile(pf); err != nil {
			pf.Close()
			return fmt.Errorf("failed to start CPU profile: %w", err)
		}
		defer pprof.StopCPUProfile()
		defer pf.Close()
		slog.Info("CPU profiling enabled", "file", f.cpuProfile)
	}

	// Write memory profile at exit if requested
	if f.memProfile != "" {
		defer func() {
			mf, err := os.Create(f.memProfile)
			if err != nil {
				slog.Error("Failed to create memory profile", "error", err)
				return
			}
			defer mf.Close()
			goruntime.GC() // Get up-to-date statistics
			if err := pprof.WriteHeapProfile(mf); err != nil {
				slog.Error("Failed to write memory profile", "error", err)
			}
			slog.Info("Memory profile written", "file", f.memProfile)
		}()
	}

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

	// Start fake proxy if --fake is specified
	fakeCleanup, err := setupFakeProxy(f.fakeResponses, f.fakeStreamDelay, &f.runConfig)
	if err != nil {
		return err
	}
	defer func() {
		if err := fakeCleanup(); err != nil {
			slog.Error("Failed to cleanup fake proxy", "error", err)
			if retErr == nil {
				retErr = fmt.Errorf("failed to cleanup fake proxy: %w", err)
			}
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
				if retErr == nil {
					retErr = fmt.Errorf("failed to cleanup recording proxy: %w", err)
				}
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
			if retErr == nil {
				retErr = fmt.Errorf("failed to close runtime: %w", err)
			}
		}
	}()
	var initialTeamCleanupOnce sync.Once
	initialTeamCleanup := func() {
		initialTeamCleanupOnce.Do(func() {
			stopToolSets(loadResult.Team)
		})
	}
	defer initialTeamCleanup()

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

	var sessStore session.Store
	switch typedRt := rt.(type) {
	case *runtime.LocalRuntime:
		sessStore = typedRt.SessionStore()
	case *runtime.PersistentRuntime:
		sessStore = typedRt.SessionStore()
	}

	return runTUI(ctx, rt, sess, f.createSessionSpawner(agentSource, sessStore), initialTeamCleanup, opts...)
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

	agent, err := t.Agent(f.agentName)
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
		sess = session.New(f.buildSessionOpts(agent.MaxIterations(), agent.ThinkingConfigured(), wd)...)
		// Session is stored lazily on first UpdateSession call (when content is added)
		// This avoids creating empty sessions in the database
		slog.Debug("Using local runtime", "agent", f.agentName, "thinking", agent.ThinkingConfigured())
	}

	return localRt, sess, nil
}

func (f *runExecFlags) handleExecMode(ctx context.Context, out *cli.Printer, rt runtime.Runtime, sess *session.Session, args []string) error {
	// args[0] is the agent file; args[1:] are user messages for multi-turn conversation
	userMessages := args[1:]

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

	return runTUI(ctx, rt, sess, nil, nil, opts...)
}

func (f *runExecFlags) buildAppOpts(args []string) ([]app.Opt, error) {
	firstMessage, err := readInitialMessage(args)
	if err != nil {
		return nil, err
	}

	var opts []app.Opt
	if firstMessage != nil {
		opts = append(opts, app.WithFirstMessage(*firstMessage))
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
func (f *runExecFlags) buildSessionOpts(maxIterations int, thinking bool, workingDir string) []session.Opt {
	return []session.Opt{
		session.WithMaxIterations(maxIterations),
		session.WithToolsApproved(f.autoApprove),
		session.WithHideToolResults(f.hideToolResults),
		session.WithThinking(thinking),
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
		agent, err := team.Agent(f.agentName)
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
		newSess := session.New(f.buildSessionOpts(agent.MaxIterations(), agent.ThinkingConfigured(), workingDir)...)

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
