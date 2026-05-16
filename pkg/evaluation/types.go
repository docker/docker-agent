package evaluation

import (
	"fmt"
	"time"

	"github.com/docker/docker-agent/pkg/session"
)

// InputSession wraps a session with its source path for evaluation loading.
type InputSession struct {
	*session.Session

	SourcePath  string // Path to the source eval file (not serialized)
	RepeatIndex int    // Repeat iteration (1-based); 0 means no repeat
}

// displayTitle returns the title with an optional repeat suffix.
func (s *InputSession) displayTitle() string {
	if s.RepeatIndex > 0 {
		return fmt.Sprintf("%s #%d", s.Title, s.RepeatIndex)
	}
	return s.Title
}

// Result contains the evaluation results for a single test case.
type Result struct {
	InputPath         string            `json:"input_path"`
	Title             string            `json:"title"`
	Question          string            `json:"question"`
	Response          string            `json:"response"`
	Cost              float64           `json:"cost"`
	OutputTokens      int64             `json:"output_tokens"`
	Size              string            `json:"size"`
	SizeExpected      string            `json:"size_expected"`
	ToolCallsScore    float64           `json:"tool_calls_score"`
	ToolCallsExpected float64           `json:"tool_calls_score_expected"`
	RelevancePassed   float64           `json:"relevance"`
	RelevanceExpected float64           `json:"relevance_expected"`
	RelevanceResults  []RelevanceResult `json:"relevance_results,omitempty"`
	Error             string            `json:"error,omitempty"`
	RawOutput         []map[string]any  `json:"raw_output,omitempty"`
	Session           *session.Session  `json:"-"` // Full session for database storage (not in JSON)
}

// checkResults returns successes and failures for this result.
func (r *Result) checkResults() (successes, failures []string) {
	if r.Error != "" {
		return nil, []string{r.Error}
	}

	// Check size
	if r.SizeExpected != "" {
		if r.SizeExpected == r.Size {
			successes = append(successes, "size "+r.Size)
		} else {
			failures = append(failures, fmt.Sprintf("size expected %s, got %s", r.SizeExpected, r.Size))
		}
	}

	// Check tool calls
	if r.ToolCallsExpected > 0 {
		if r.ToolCallsScore >= 1.0 {
			successes = append(successes, "tool calls")
		} else {
			failures = append(failures, fmt.Sprintf("tool calls score %.2f", r.ToolCallsScore))
		}
	}

	// Check relevance
	if r.RelevanceExpected > 0 {
		if r.RelevancePassed >= r.RelevanceExpected {
			successes = append(successes, fmt.Sprintf("relevance %.0f/%.0f", r.RelevancePassed, r.RelevanceExpected))
		} else {
			for _, result := range r.RelevanceResults {
				if !result.Passed {
					if result.Reason != "" {
						failures = append(failures, fmt.Sprintf("relevance: %s (reason: %s)", result.Criterion, result.Reason))
					} else {
						failures = append(failures, "relevance: "+result.Criterion)
					}
				}
			}
		}
	}

	return successes, failures
}

// Summary contains aggregate statistics across all evaluations.
type Summary struct {
	TotalEvals      int     `json:"total_evals"`
	FailedEvals     int     `json:"failed_evals"`
	TotalCost       float64 `json:"total_cost"`
	SizesPassed     int     `json:"sizes_passed"`
	SizesTotal      int     `json:"sizes_total"`
	ToolsF1Sum      float64 `json:"tools_f1_sum"`
	ToolsCount      int     `json:"tools_count"`
	RelevancePassed float64 `json:"relevance_passed"`
	RelevanceTotal  float64 `json:"relevance_total"`
}

// EvalRun contains the results and metadata for an evaluation run.
type EvalRun struct {
	Name      string        `json:"name"`
	Timestamp time.Time     `json:"timestamp"`
	Duration  time.Duration `json:"duration"`
	Config    Config        `json:"-"` // Used to build RunOutput, not serialized directly
	Results   []Result      `json:"results"`
	Summary   Summary       `json:"summary"`
}

// RunOutput is the top-level structure for the evaluation run JSON output.
type RunOutput struct {
	Name      string             `json:"name"`
	Timestamp time.Time          `json:"timestamp"`
	Duration  string             `json:"duration"`
	Config    RunOutputConfig    `json:"config"`
	Summary   Summary            `json:"summary"`
	Sessions  []*session.Session `json:"sessions"`
}

// RunOutputConfig captures the evaluation run configuration.
type RunOutputConfig struct {
	Agent       string `json:"agent"`
	JudgeModel  string `json:"judge_model,omitempty"`
	Concurrency int    `json:"concurrency"`
	EvalsDir    string `json:"evals_dir"`
	BaseImage   string `json:"base_image,omitempty"`
}

// Config holds configuration for evaluation runs.
type Config struct {
	AgentFilename  string   // Path to the agent configuration file
	EvalsDir       string   // Directory containing evaluation files
	JudgeModel     string   // Model for relevance checking (format: provider/model, optional)
	Concurrency    int      // Number of concurrent runs (0 = number of CPUs)
	TTYFd          int      // File descriptor for terminal size queries (e.g., int(os.Stdout.Fd()))
	Only           []string // Only run evaluations matching these patterns
	BaseImage      string   // Custom base Docker image for running evaluations
	KeepContainers bool     // If true, don't remove containers after evaluation (skip --rm)
	EnvVars        []string // Environment variables to pass: KEY (value from env) or KEY=VALUE (explicit)
	Repeat         int      // Number of times to repeat each evaluation (default 1)
}

// Session helper functions

func getUserMessages(sess *session.Session) []string {
	var messages []string
	for _, msg := range sess.GetAllMessages() {
		if msg.Message.Role == "user" {
			messages = append(messages, msg.Message.Content)
		}
	}
	return messages
}

func extractToolCalls(items []session.Item) []string {
	var names []string
	for _, item := range items {
		if item.Message != nil {
			for _, tc := range item.Message.Message.ToolCalls {
				names = append(names, tc.Function.Name)
			}
		}
		if item.SubSession != nil {
			names = append(names, extractToolCalls(item.SubSession.Messages)...)
		}
	}
	return names
}
