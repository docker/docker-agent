package root

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/evaluation"
	"github.com/docker/docker-agent/pkg/model/provider/providers"
	"github.com/docker/docker-agent/pkg/telemetry"
)

const defaultJudgeModel = "anthropic/claude-opus-5"

type evalFlags struct {
	evaluation.Config

	runConfig config.RuntimeConfig
	outputDir string

	// baseline is a previously saved run (an -eval.json written by a prior
	// invocation) to compare this run against; empty disables the check.
	baseline string
	// regressionTolerance is how far an aggregate quality rate may fall before
	// the comparison fails. See evaluation.Compare for the exact semantics.
	regressionTolerance float64
}

func newEvalCmd() *cobra.Command {
	var flags evalFlags

	cmd := &cobra.Command{
		Use:     "eval <agent-file>|<registry-ref> [<eval-dir>|./evals]",
		Short:   "Run evaluations for an agent",
		GroupID: "advanced",
		Args:    cobra.RangeArgs(1, 2),
		RunE:    flags.runEvalCommand,
	}

	addRuntimeConfigFlags(cmd, &flags.runConfig)
	cmd.Flags().IntVarP(&flags.Concurrency, "concurrency", "c", runtime.NumCPU(), "Number of concurrent evaluation runs")
	cmd.Flags().StringVar(&flags.JudgeModel, "judge-model", defaultJudgeModel, "Model to use for relevance checking (format: provider/model)")
	cmd.Flags().StringVar(&flags.outputDir, "output", "", "Directory for results and logs (default: <eval-dir>/results)")
	cmd.Flags().StringSliceVar(&flags.Only, "only", nil, "Only run evaluations with file names matching these patterns (can be specified multiple times)")
	cmd.Flags().StringVar(&flags.BaseImage, "base-image", "", "Custom base image for running evaluations")
	cmd.Flags().StringVar(&flags.AgentImage, "agent-image", "",
		"docker-agent image to inject into eval containers (default: pinned to this CLI's own release version, e.g. docker/docker-agent:1.2.3; falls back to docker/docker-agent:edge for dev builds); pass \"none\" to skip injection and trust the base image's own binary")
	cmd.Flags().StringVar(&flags.ContainerRuntime, "container-runtime", evaluation.DefaultContainerRuntime, "Container runtime executable for building and running evaluations")
	cmd.Flags().BoolVar(&flags.KeepContainers, "keep-containers", false, "Keep containers after evaluation (don't use --rm)")
	cmd.Flags().StringSliceVarP(&flags.EnvVars, "env", "e", nil, "Environment variables to pass to container (KEY or KEY=VALUE)")
	cmd.Flags().IntVar(&flags.Repeat, "repeat", 1, "Number of times to repeat each evaluation (useful for computing baselines)")
	cmd.Flags().StringVar(&flags.baseline, "baseline", "", "Compare against a previously saved run JSON (<output>/<run>.json) and exit non-zero on regression")
	cmd.Flags().Float64Var(&flags.regressionTolerance, "regression-tolerance", 0, "How far an aggregate quality rate may fall before --baseline reports a regression (0-1)")

	return cmd
}

func (f *evalFlags) runEvalCommand(cmd *cobra.Command, args []string) (commandErr error) {
	if f.regressionTolerance > evaluation.MaxTolerance {
		return fmt.Errorf("--regression-tolerance must be between 0 and %v; %v would disable the aggregate gate",
			evaluation.MaxTolerance, f.regressionTolerance)
	}

	telemetry.TrackCommand(cmd.Context(), "eval", args)
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(cmd.Context(), "eval", args, commandErr)
	}()

	ctx := cmd.Context()
	agentFilename := args[0]
	evalsDir := "./evals"
	if len(args) >= 2 {
		evalsDir = args[1]
	}

	// Output directory defaults to <evals-dir>/results
	outputDir := f.outputDir
	if outputDir == "" {
		outputDir = filepath.Join(evalsDir, "results")
	}

	// Create output directory
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	// Generate run name upfront so we can set up logging
	runName := evaluation.GenerateRunName()

	// Set up log file with debug logging
	logPath := filepath.Join(outputDir, runName+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("creating log file: %w", err)
	}
	defer logFile.Close()

	// Set up slog to write debug logs to the log file
	logHandler := slog.NewTextHandler(logFile, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(logHandler))
	defer slog.SetDefault(originalLogger)

	// Write header to log file
	fmt.Fprintf(logFile, "=== Evaluation Run: %s ===\n", runName)
	fmt.Fprintf(logFile, "Started: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(logFile, "Agent: %s\n", agentFilename)
	fmt.Fprintf(logFile, "Evals dir: %s\n", evalsDir)
	fmt.Fprintf(logFile, "Judge model: %s\n", f.JudgeModel)
	fmt.Fprintf(logFile, "Concurrency: %d\n", f.Concurrency)
	fmt.Fprintf(logFile, "Container runtime: %s\n", f.ContainerRuntime)
	if agentImage := evaluation.ResolvedAgentImage(f.Config); agentImage != "" {
		fmt.Fprintf(logFile, "Agent image: %s\n", agentImage)
	} else {
		fmt.Fprintf(logFile, "Agent image: (none, trusting base image binary)\n")
	}
	fmt.Fprintf(logFile, "\n")

	// Create tee writer to write to both console and log file
	consoleOut := cmd.OutOrStdout()
	teeOut := io.MultiWriter(consoleOut, logFile)

	// Check if console is a TTY (for colored output)
	isTTY := false
	if file, ok := consoleOut.(*os.File); ok {
		f.TTYFd = int(file.Fd())
		isTTY = term.IsTerminal(f.TTYFd)
	}

	// Set remaining config fields
	f.AgentFilename = agentFilename
	f.EvalsDir = evalsDir

	// Wire the full provider set so the judge model can be built (the package
	// default registry is empty; see pkg/model/provider/providers).
	f.runConfig.ProviderRegistry = providers.NewDefaultRegistry()

	// Run evaluation
	// Pass consoleOut for TTY progress bar, teeOut for results that should go to both console and log
	run, evalErr := evaluation.Evaluate(ctx, consoleOut, teeOut, isTTY, runName, &f.runConfig, f.Config)
	if run == nil {
		return evalErr
	}

	// Save sessions to SQLite database
	dbPath, err := evaluation.SaveRunSessions(ctx, run, outputDir)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to save sessions database", "error", err)
	} else {
		fmt.Fprintf(teeOut, "\nSessions DB: %s\n", dbPath)
	}

	// Save sessions to JSON file (same format as /eval produces)
	sessionsPath, err := evaluation.SaveRunSessionsJSON(run, outputDir)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to save sessions JSON", "error", err)
	} else {
		fmt.Fprintf(teeOut, "Sessions JSON: %s\n", sessionsPath)
	}

	fmt.Fprintf(teeOut, "Log: %s\n", logPath)

	// Only compare a run that completed. A partial run's missing evaluations
	// register as "absent" and do not gate, so comparing would print
	// "✅ No regression against baseline" for a broken run and then exit
	// non-zero — contradictory, and the reassuring half is the one people read.
	if evalErr != nil {
		if f.baseline != "" {
			fmt.Fprintln(teeOut, "\nSkipping baseline comparison: the run did not complete.")
		}
		return evalErr
	}

	return f.checkBaseline(teeOut, run)
}

// checkBaseline compares run against the configured baseline and returns a
// non-nil error when it regressed, so CI fails on the exit code. A no-op when
// --baseline was not supplied.
func (f *evalFlags) checkBaseline(out io.Writer, run *evaluation.EvalRun) error {
	if f.baseline == "" {
		return nil
	}

	baseline, err := evaluation.LoadBaseline(f.baseline)
	if err != nil {
		return err
	}

	comparison, err := evaluation.Compare(baseline, run, f.regressionTolerance)
	if err != nil {
		return err
	}
	evaluation.PrintComparison(out, comparison)

	if comparison.Regressed {
		return errors.New("evaluation regressed against baseline")
	}
	return nil
}
