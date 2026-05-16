// Package evaluation provides an evaluation framework for testing agents.
package evaluation

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/session"
)

// Runner runs evaluations against an agent.
type Runner struct {
	Config

	agentSource config.Source
	judge       *Judge
	runConfig   *config.RuntimeConfig

	// imageCache caches built Docker images by (workingDir, image) pair.
	imageCache   map[imageKey]string
	imageCacheMu sync.Mutex

	// imageBuildGroup deduplicates concurrent image builds for the same (workingDir, image) pair.
	imageBuildGroup singleflight.Group
}

// newRunner creates a new evaluation runner.
func newRunner(agentSource config.Source, runConfig *config.RuntimeConfig, judgeModel provider.Provider, cfg Config) *Runner {
	var judge *Judge
	if judgeModel != nil {
		judge = NewJudge(judgeModel, cfg.Concurrency)
	}
	return &Runner{
		Config:      cfg,
		agentSource: agentSource,
		judge:       judge,
		runConfig:   runConfig,
		imageCache:  make(map[imageKey]string),
	}
}

// Evaluate runs evaluations with a specified run name.
// ttyOut is used for progress bar rendering (should be the console/TTY).
// out is used for results and status messages (can be tee'd to a log file).
func Evaluate(ctx context.Context, ttyOut, out io.Writer, isTTY bool, runName string, runConfig *config.RuntimeConfig, cfg Config) (*EvalRun, error) {
	agentSource, err := config.Resolve(cfg.AgentFilename, nil)
	if err != nil {
		return nil, fmt.Errorf("resolving agent: %w", err)
	}

	// Create judge model provider for relevance checking
	judgeModel, err := createJudgeModel(ctx, cfg.JudgeModel, runConfig)
	if err != nil {
		return nil, err
	}

	runner := newRunner(agentSource, runConfig, judgeModel, cfg)

	fmt.Fprintf(out, "Evaluation run: %s\n", runName)

	startTime := time.Now()
	results, err := runner.Run(ctx, ttyOut, out, isTTY)
	duration := time.Since(startTime)

	summary := computeSummary(results)
	printSummary(out, summary, duration)

	run := &EvalRun{
		Name:      runName,
		Timestamp: startTime,
		Duration:  duration,
		Config:    cfg,
		Results:   results,
		Summary:   summary,
	}

	if err != nil {
		return run, fmt.Errorf("running evaluations: %w", err)
	}

	return run, nil
}

// workItem represents a single evaluation to be processed.
type workItem struct {
	index int
	eval  *InputSession
}

// Run executes all evaluations concurrently and returns results.
// ttyOut is used for progress bar rendering (should be the console/TTY).
// out is used for results and status messages (can be tee'd to a log file).
func (r *Runner) Run(ctx context.Context, ttyOut, out io.Writer, isTTY bool) ([]Result, error) {
	fmt.Fprintln(out, "Loading evaluation sessions...")
	evals, err := r.loadEvalSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading evaluations: %w", err)
	}

	// Check whether any evaluations require relevance checking.
	// If so, the judge must be configured and working; validate eagerly
	// to fail fast on configuration issues (bad API key, wrong model, etc.)
	// instead of silently producing zero-relevance results.
	if needsJudge(evals) {
		if r.judge == nil {
			return nil, errors.New("some evaluations have relevance criteria but no judge model is configured (use --judge-model)")
		}
		fmt.Fprintln(out, "Validating judge model...")
		if err := r.judge.Validate(ctx); err != nil {
			return nil, fmt.Errorf("%w", err)
		}
	}

	// Pre-build all unique Docker images in parallel before running evaluations.
	// This avoids serialized builds when multiple workers need the same image.
	if err := r.preBuildImages(ctx, out, evals); err != nil {
		return nil, fmt.Errorf("pre-building images: %w", err)
	}

	fmt.Fprintf(out, "Running %d evaluations with concurrency %d\n\n", len(evals), r.Concurrency)

	progress := newProgressBar(ttyOut, out, r.TTYFd, len(evals), isTTY)
	progress.start()
	defer progress.stop()

	results := make([]Result, len(evals))

	work := make(chan workItem, len(evals))
	for i := range evals {
		work <- workItem{index: i, eval: &evals[i]}
	}
	close(work)

	var wg sync.WaitGroup
	for range r.Concurrency {
		wg.Go(func() {
			for item := range work {
				if ctx.Err() != nil {
					return
				}

				progress.setRunning(item.eval.displayTitle())
				result, runErr := r.runSingleEval(ctx, item.eval)
				if runErr != nil {
					result.Error = runErr.Error()
					slog.ErrorContext(ctx, "Evaluation failed", "title", item.eval.displayTitle(), "error", runErr)
				}

				results[item.index] = result
				_, failures := result.checkResults()
				progress.complete(result.Title, len(failures) == 0)
				progress.printResult(result)
			}
		})
	}

	wg.Wait()

	if ctx.Err() != nil {
		return results, ctx.Err()
	}

	return results, nil
}

func (r *Runner) loadEvalSessions(ctx context.Context) ([]InputSession, error) {
	entries, err := os.ReadDir(r.EvalsDir)
	if err != nil {
		return nil, err
	}

	var evals []InputSession
	for _, entry := range entries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Filter by --only patterns against file name if specified
		fileName := entry.Name()
		if len(r.Only) > 0 && !matchesAnyPattern(fileName, r.Only) {
			continue
		}

		if entry.IsDir() || !strings.HasSuffix(fileName, ".json") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(r.EvalsDir, fileName))
		if err != nil {
			return nil, err
		}

		var evalSess session.Session
		if err := json.Unmarshal(data, &evalSess); err != nil {
			return nil, err
		}

		evals = append(evals, InputSession{
			Session:    &evalSess,
			SourcePath: filepath.Join(r.EvalsDir, fileName),
		})
	}

	// Sort by duration (longest first) to avoid long tail
	slices.SortFunc(evals, func(a, b InputSession) int {
		return cmp.Compare(b.Duration(), a.Duration())
	})

	// Repeat evals if requested
	repeat := max(r.Repeat, 1)
	if repeat > 1 {
		original := evals
		evals = make([]InputSession, 0, len(original)*repeat)
		for i := range repeat {
			for _, e := range original {
				evals = append(evals, InputSession{
					Session:     e.Session,
					SourcePath:  e.SourcePath,
					RepeatIndex: i + 1,
				})
			}
		}
	}

	return evals, nil
}

// preBuildImages pre-builds all unique Docker images needed for the evaluations.
// Concurrent calls for the same (workingDir, image) pair are deduplicated by
// getOrBuildImage's singleflight, so we simply iterate over all evals.
func (r *Runner) preBuildImages(ctx context.Context, out io.Writer, evals []InputSession) error {
	if len(evals) == 0 {
		return nil
	}

	// Count unique images to report an accurate number.
	unique := make(map[imageKey]struct{})
	for _, eval := range evals {
		var key imageKey
		if eval.Evals != nil {
			key = imageKey{workingDir: eval.Evals.WorkingDir, image: eval.Evals.Image}
		}
		unique[key] = struct{}{}
	}

	fmt.Fprintf(out, "Pre-building %d Docker image(s)...\n", len(unique))

	type buildResult struct {
		title string
		err   error
	}

	work := make(chan InputSession, len(evals))
	for _, eval := range evals {
		work <- eval
	}
	close(work)

	results := make(chan buildResult, len(evals))

	buildWorkers := min(r.Concurrency, len(evals))
	var wg sync.WaitGroup
	for range buildWorkers {
		wg.Go(func() {
			for eval := range work {
				if ctx.Err() != nil {
					results <- buildResult{title: eval.Title, err: ctx.Err()}
					continue
				}

				criteria := eval.Evals
				if criteria == nil {
					criteria = &session.EvalCriteria{}
				}

				_, err := r.getOrBuildImage(ctx, criteria)
				results <- buildResult{title: eval.Title, err: err}
			}
		})
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var errs []error
	for result := range results {
		if result.err != nil {
			errs = append(errs, fmt.Errorf("building image for %q: %w", result.title, result.err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to build %d image(s): %w", len(errs), errs[0])
	}

	return nil
}

func (r *Runner) runSingleEval(ctx context.Context, evalSess *InputSession) (Result, error) {
	startTime := time.Now()
	title := evalSess.displayTitle()

	slog.DebugContext(ctx, "Starting evaluation", "title", title)

	var evals *session.EvalCriteria
	if evalSess.Evals != nil {
		evals = evalSess.Evals
	} else {
		evals = &session.EvalCriteria{}
	}

	userMessages := getUserMessages(evalSess.Session)

	result := Result{
		InputPath:         evalSess.SourcePath,
		Title:             title,
		Question:          strings.Join(userMessages, "\n"),
		SizeExpected:      evals.Size,
		RelevanceExpected: float64(len(evals.Relevance)),
	}

	expectedToolCalls := extractToolCalls(evalSess.Messages)
	if len(expectedToolCalls) > 0 {
		result.ToolCallsExpected = 1.0
	}

	imageID, err := r.getOrBuildImage(ctx, evals)
	if err != nil {
		return result, fmt.Errorf("building eval image: %w", err)
	}

	events, err := r.runDockerAgentInContainer(ctx, imageID, userMessages, evals.Setup)
	if err != nil {
		return result, fmt.Errorf("running docker agent in container: %w", err)
	}

	response, cost, outputTokens, actualToolCalls := parseContainerEvents(events)

	result.Response = response
	result.Cost = cost
	result.OutputTokens = outputTokens
	result.Size = getResponseSize(result.Response)

	// Build session from events for database storage
	result.Session = SessionFromEvents(events, title, userMessages)
	result.Session.Evals = evals

	// Re-apply the display title in case a session_title event overrode it.
	// This ensures repeated evals retain their '#N' suffix in stored sessions.
	result.Session.Title = title

	if len(expectedToolCalls) > 0 || len(actualToolCalls) > 0 {
		result.ToolCallsScore = toolCallF1Score(expectedToolCalls, actualToolCalls)
	}

	if r.judge != nil && len(evals.Relevance) > 0 {
		// Use transcript for relevance checking to preserve temporal ordering
		transcript := buildTranscript(events)
		results, err := r.judge.CheckRelevance(ctx, transcript, evals.Relevance)
		if err != nil {
			return result, fmt.Errorf("relevance check failed: %w", err)
		}
		var passed float64
		for _, rr := range results {
			if rr.Passed {
				passed++
			}
		}
		result.RelevancePassed = passed
		result.RelevanceResults = results
	}

	slog.DebugContext(ctx, "Evaluation complete", "title", title, "duration", time.Since(startTime))
	return result, nil
}

func (r *Runner) runDockerAgentInContainer(ctx context.Context, imageID string, questions []string, setup string) ([]map[string]any, error) {
	agentDir := r.agentSource.ParentDir()
	agentFile := filepath.Base(r.agentSource.Name())
	containerName := fmt.Sprintf("docker-agent-eval-%d", uuid.New().ID())

	args := []string{
		"run",
		"--name", containerName,
		"--privileged",
		"--init",
	}
	if !r.KeepContainers {
		args = append(args, "--rm")
	}
	args = append(args,
		"-i",
		"-v", agentDir+":/configs:ro",
	)

	var env []string

	if r.runConfig.ModelsGateway != "" {
		args = append(args, "-e", "DOCKER_AGENT_MODELS_GATEWAY")
		env = append(env, "DOCKER_AGENT_MODELS_GATEWAY="+r.runConfig.ModelsGateway)

		if token, ok := r.runConfig.EnvProvider().Get(ctx, environment.DockerDesktopTokenEnv); ok && token != "" {
			args = append(args, "-e", environment.DockerDesktopTokenEnv)
			env = append(env, environment.DockerDesktopTokenEnv+"="+token)
		}
	} else {
		for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GOOGLE_API_KEY", "MISTRAL_API_KEY", "XAI_API_KEY", "NEBIUS_API_KEY"} {
			if val, ok := r.runConfig.EnvProvider().Get(ctx, name); ok && val != "" {
				args = append(args, "-e", name)
				env = append(env, name+"="+val)
			}
		}
	}

	// Pass additional environment variables specified via -e flag
	// Format: KEY or KEY=VALUE
	for _, entry := range r.EnvVars {
		if key, val, hasValue := strings.Cut(entry, "="); hasValue && key != "" {
			args = append(args, "-e", key)
			env = append(env, key+"="+val)
		} else if val, ok := r.runConfig.EnvProvider().Get(ctx, entry); ok && entry != "" {
			args = append(args, "-e", entry)
			env = append(env, entry+"="+val)
		}
	}

	// When a setup script is provided, mount it into the container and
	// override the entrypoint to run it before docker agent run --exec.
	// The default entrypoint is: /run.sh /docker-agent run --exec --yolo --json
	// /run.sh starts dockerd then exec's "$@".
	if setup != "" {
		setupFile := filepath.Join(os.TempDir(), fmt.Sprintf("docker-agent-eval-setup-%d.sh", uuid.New().ID()))
		if err := os.WriteFile(setupFile, []byte(setup), 0o600); err != nil {
			return nil, fmt.Errorf("writing setup script: %w", err)
		}
		defer os.Remove(setupFile)

		args = append(args,
			"-v", setupFile+":/setup.sh:ro",
			"--entrypoint", "/run.sh",
		)
	}

	args = append(args, imageID)

	if setup != "" {
		// Run setup script, then docker agent run --exec with the original arguments.
		args = append(args, "sh", "-c", "sh /setup.sh && exec /docker-agent run --exec --yolo --json \"$@\"", "--", "/configs/"+agentFile)
	} else {
		args = append(args, "/configs/"+agentFile)
	}
	args = append(args, questions...)

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(env, os.Environ()...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting docker run: %w", err)
	}

	var stderrData []byte
	go func() {
		stderrData, _ = io.ReadAll(stderr)
	}()

	var events []map[string]any
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			slog.DebugContext(ctx, "Failed to parse JSON event", "line", line, "error", err)
			continue
		}
		events = append(events, event)
	}

	if err := scanner.Err(); err != nil {
		slog.WarnContext(ctx, "Error reading container output", "error", err)
	}

	waitErr := cmd.Wait()
	if waitErr != nil {
		slog.DebugContext(ctx, "Container exited with error", "stderr", string(stderrData), "error", waitErr)
	}

	if len(events) == 0 {
		stderrStr := strings.TrimSpace(string(stderrData))
		if waitErr != nil {
			return nil, fmt.Errorf("container failed: %w (stderr: %s)", waitErr, stderrStr)
		}
		if stderrStr != "" {
			return nil, fmt.Errorf("no events received from container (stderr: %s)", stderrStr)
		}
		return nil, errors.New("no events received from container")
	}

	return events, nil
}

func parseContainerEvents(events []map[string]any) (response string, cost float64, outputTokens int64, toolCalls []string) {
	var responseBuf strings.Builder
	for _, event := range events {
		eventType, _ := event["type"].(string)

		switch eventType {
		case "agent_choice":
			if content, ok := event["content"].(string); ok {
				responseBuf.WriteString(content)
			}
		case "tool_call":
			if tc, ok := event["tool_call"].(map[string]any); ok {
				if fn, ok := tc["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok {
						toolCalls = append(toolCalls, name)
					}
				}
			}
		case "token_usage":
			if usage, ok := event["usage"].(map[string]any); ok {
				if c, ok := usage["cost"].(float64); ok {
					cost = c
				}
				if tokens, ok := usage["output_tokens"].(float64); ok {
					outputTokens += int64(tokens)
				}
			}
		}
	}

	return responseBuf.String(), cost, outputTokens, toolCalls
}

// buildTranscript creates a chronological transcript of agent interactions.
// Unlike parseContainerEvents which only extracts text, this preserves the
// temporal sequence of events, enabling evaluation of criteria like
// "explains before executing" or "announces tool usage beforehand".
func buildTranscript(events []map[string]any) string {
	var transcript strings.Builder
	var pendingText strings.Builder
	var currentAgent string

	flushText := func() {
		if pendingText.Len() == 0 {
			return
		}
		fmt.Fprintf(&transcript, "[Agent %s says]:\n%s\n\n", cmp.Or(currentAgent, "unknown"), pendingText.String())
		pendingText.Reset()
	}

	for _, event := range events {
		switch event["type"] {
		case "agent_choice":
			if agentName, _ := event["agent_name"].(string); agentName != "" {
				currentAgent = agentName
			}
			if content, _ := event["content"].(string); content != "" {
				pendingText.WriteString(content)
			}

		case "tool_call":
			flushText()
			name, args := getToolCallInfo(event)
			if agentName, _ := event["agent_name"].(string); agentName != "" {
				currentAgent = agentName
			}
			fmt.Fprintf(&transcript, "[Agent %s calls tool %q with arguments: %s]\n\n", cmp.Or(currentAgent, "unknown"), name, args)

		case "tool_call_response":
			// The ToolCallResponseEvent has tool_definition at the top level, not
			// nested under "tool_call".
			var name string
			if td, ok := event["tool_definition"].(map[string]any); ok {
				name, _ = td["name"].(string)
			}
			response, _ := event["response"].(string)
			if len(response) > 500 {
				response = response[:500] + "...(truncated)"
			}
			fmt.Fprintf(&transcript, "[Tool %q returns: %s]\n\n", name, response)
		}
	}

	flushText()
	return transcript.String()
}

// getToolCallInfo extracts the tool name and arguments from an event.
func getToolCallInfo(event map[string]any) (name, args string) {
	tc, _ := event["tool_call"].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	name, _ = fn["name"].(string)
	args, _ = fn["arguments"].(string)
	return name, args
}

// matchesAnyPattern returns true if the name contains any of the patterns (case-insensitive).
func matchesAnyPattern(name string, patterns []string) bool {
	nameLower := strings.ToLower(name)
	return slices.ContainsFunc(patterns, func(pattern string) bool {
		return strings.Contains(nameLower, strings.ToLower(pattern))
	})
}

// needsJudge returns true if any evaluation session has relevance criteria,
// meaning a judge model is required to evaluate them.
func needsJudge(evals []InputSession) bool {
	return slices.ContainsFunc(evals, func(s InputSession) bool {
		return s.Evals != nil && len(s.Evals.Relevance) > 0
	})
}

// createJudgeModel creates a provider.Provider from a model string (format: provider/model).
// Returns nil if judgeModel is empty.
func createJudgeModel(ctx context.Context, judgeModel string, runConfig *config.RuntimeConfig) (provider.Provider, error) {
	if judgeModel == "" {
		return nil, nil
	}

	cfg, err := latest.ParseModelRef(judgeModel)
	if err != nil {
		return nil, fmt.Errorf("invalid judge model format %q: expected 'provider/model'", judgeModel)
	}

	opts := []options.Opt{
		options.WithStructuredOutput(judgeResponseSchema),
	}
	if runConfig.ModelsGateway != "" {
		opts = append(opts, options.WithGateway(runConfig.ModelsGateway))
	}

	judge, err := provider.New(ctx, &cfg, runConfig.EnvProvider(), opts...)
	if err != nil {
		return nil, fmt.Errorf("creating judge model: %w", err)
	}

	return judge, nil
}
