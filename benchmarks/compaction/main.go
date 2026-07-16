// Command bench-compaction benchmarks compaction summarization models
// against real docker-agent sessions. It replicates the runtime's compaction
// input pipeline (session.CompactionInput + keep/context budgets + the
// canonical compaction prompts), then runs each candidate model through
// OpenRouter and records speed, token usage, cost and the summary text.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/providers"
	"github.com/docker/docker-agent/pkg/session"
)

// contextLimit is fixed to haiku's window (the smallest of the three) so
// every model summarizes the exact same input.
const (
	contextLimit     = 200_000
	maxSummaryTokens = 16_000 // compactor.MaxSummaryTokens
	maxKeepTokens    = 20_000 // compactor.maxKeepTokens
)

type modelSpec struct {
	Name      string  // short name used in reports/files
	Model     string  // OpenRouter model id
	InPerTok  float64 // $ per input token
	OutPerTok float64 // $ per output token
	Effort    string  // reasoning effort override ("" = provider default)
}

var specs = []modelSpec{
	{"gpt-5-nano", "openai/gpt-5-nano", 0.00000005, 0.0000004, ""},
	{"gpt-5-nano-min", "openai/gpt-5-nano", 0.00000005, 0.0000004, "minimal"},
	{"gpt-5.4-nano", "openai/gpt-5.4-nano", 0.0000002, 0.00000125, ""},
	{"haiku-4.5", "anthropic/claude-haiku-4.5", 0.000001, 0.000005, ""},
}

type candidate struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Bytes int64  `json:"bytes"`
	Size  string `json:"size"`
	Tools string `json:"tools"`
}

type runResult struct {
	Model           string  `json:"model"`
	Summary         string  `json:"summary"`
	Error           string  `json:"error,omitempty"`
	TTFTSeconds     float64 `json:"ttft_seconds"`
	TotalSeconds    float64 `json:"total_seconds"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	ReasoningTokens int64   `json:"reasoning_tokens"`
	CostUSD         float64 `json:"cost_usd"`
}

type sessionResult struct {
	SessionID      string      `json:"session_id"`
	Title          string      `json:"title"`
	Size           string      `json:"size"`
	Tools          string      `json:"tools"`
	Bytes          int64       `json:"bytes"`
	InputMessages  int         `json:"input_messages"`
	InputEstTokens int64       `json:"input_est_tokens"`
	Runs           []runResult `json:"runs"`
}

func main() {
	cmd := "bench"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "bench":
		err = run(context.Background())
	case "judge":
		err = runJudge(context.Background())
	default:
		err = fmt.Errorf("unknown command %q (want bench or judge)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	raw, err := os.ReadFile("selected.json")
	if err != nil {
		return err
	}
	var candidates []candidate
	if err := json.Unmarshal(raw, &candidates); err != nil {
		return err
	}

	store, err := session.NewSQLiteSessionStore(ctx, "session-copy.db")
	if err != nil {
		return err
	}
	defer store.Close()

	if err := os.MkdirAll("results", 0o755); err != nil {
		return err
	}

	env := environment.NewDefaultProvider()
	models := make(map[string]provider.Provider, len(specs))
	for _, spec := range specs {
		cfg := &latest.ModelConfig{
			Provider:            "openrouter",
			Model:               spec.Model,
			BypassModelsGateway: true,
			TrackUsage:          boolPtr(true),
			ProviderOpts:        map[string]any{"api_type": "openai_chatcompletions"},
		}
		if spec.Effort != "" {
			cfg.ThinkingBudget = &latest.ThinkingBudget{Effort: spec.Effort}
		}
		// cagent's openai client doesn't send reasoning_effort for
		// provider-prefixed ids (modelinfo.UsesReasoningEffort doesn't strip
		// "openai/"), so inject OpenRouter's normalized reasoning field at the
		// HTTP layer instead.
		opts := []options.Opt{options.WithMaxTokens(maxSummaryTokens)}
		if spec.Effort != "" {
			opts = append(opts, options.WithHTTPTransportWrapper(injectReasoningEffort(spec.Effort)))
		}
		p, err := providers.NewDefaultRegistry().New(ctx, cfg, env, opts...)
		if err != nil {
			return fmt.Errorf("provider %s: %w", spec.Name, err)
		}
		models[spec.Name] = p
	}

	for i, cand := range candidates {
		outPath := filepath.Join("results", cand.ID+".json")

		// Reuse an existing result file and only run specs it is missing,
		// so new model variants can be appended without re-running the rest.
		var existing *sessionResult
		if raw, err := os.ReadFile(outPath); err == nil {
			existing = &sessionResult{}
			if err := json.Unmarshal(raw, existing); err != nil {
				return fmt.Errorf("parse %s: %w", outPath, err)
			}
		}
		done := map[string]bool{}
		if existing != nil {
			for _, r := range existing.Runs {
				done[r.Model] = true
			}
		}
		missing := false
		for _, spec := range specs {
			if !done[spec.Name] {
				missing = true
			}
		}
		if !missing {
			fmt.Printf("[%d/%d] %s already done, skipping\n", i+1, len(candidates), cand.ID)
			continue
		}

		sess, err := store.GetSession(ctx, cand.ID)
		if err != nil {
			fmt.Printf("[%d/%d] %s: load failed: %v\n", i+1, len(candidates), cand.ID, err)
			continue
		}
		messages := buildCompactionMessages(sess)
		if len(messages) <= 2 {
			fmt.Printf("[%d/%d] %s: nothing to compact, skipping\n", i+1, len(candidates), cand.ID)
			continue
		}
		var estTokens int64
		for i := range messages {
			estTokens += compaction.EstimateMessageTokens(&messages[i])
		}

		res := sessionResult{
			SessionID:      cand.ID,
			Title:          cand.Title,
			Size:           cand.Size,
			Tools:          cand.Tools,
			Bytes:          cand.Bytes,
			InputMessages:  len(messages),
			InputEstTokens: estTokens,
		}
		if existing != nil {
			res.Runs = existing.Runs
		}
		fmt.Printf("[%d/%d] %s (%s/%s, ~%dk tokens, %d msgs) %q\n",
			i+1, len(candidates), cand.ID, cand.Size, cand.Tools, estTokens/1000, len(messages), truncate(cand.Title, 50))

		for _, spec := range specs {
			if done[spec.Name] {
				continue
			}
			r := summarize(ctx, models[spec.Name], spec, messages)
			res.Runs = append(res.Runs, r)
			status := fmt.Sprintf("%.1fs ttft=%.1fs in=%d out=%d $%.5f", r.TotalSeconds, r.TTFTSeconds, r.InputTokens, r.OutputTokens, r.CostUSD)
			if r.Error != "" {
				status = "ERROR: " + r.Error
			}
			fmt.Printf("    %-14s %s\n", spec.Name, status)
		}

		data, _ := json.MarshalIndent(res, "", " ")
		if err := os.WriteFile(outPath, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// buildCompactionMessages mirrors compactor.extractMessages: the session's
// compaction input trimmed of its keep-tail, truncated to the context budget,
// wrapped in the canonical compaction system/user prompts.
func buildCompactionMessages(sess *session.Session) []chat.Message {
	messages, _, _ := sess.CompactionInput()
	for i := range messages {
		messages[i].Cost = 0
	}

	splitIdx := compaction.SplitIndexForKeep(messages, min(maxKeepTokens, contextLimit/5))
	messages = messages[:splitIdx]

	systemPromptMessage := chat.Message{
		Role:      chat.MessageRoleSystem,
		Content:   compaction.SystemPrompt,
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	userPromptMessage := chat.Message{
		Role:      chat.MessageRoleUser,
		Content:   compaction.UserPrompt,
		CreatedAt: time.Now().Format(time.RFC3339),
	}

	contextAvailable := max(int64(0),
		contextLimit-min(maxSummaryTokens, contextLimit/4)-
			compaction.EstimateMessageTokens(&systemPromptMessage)-
			compaction.EstimateMessageTokens(&userPromptMessage))
	firstIndex := compaction.FirstIndexInBudget(messages, contextAvailable)
	if firstIndex < len(messages) {
		messages = messages[firstIndex:]
	} else {
		messages = nil
	}

	messages = append([]chat.Message{systemPromptMessage}, messages...)
	return append(messages, userPromptMessage)
}

func summarize(ctx context.Context, p provider.Provider, spec modelSpec, messages []chat.Message) runResult {
	res := runResult{Model: spec.Name}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	start := time.Now()
	stream, err := p.CreateChatCompletionStream(ctx, messages, nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer stream.Close()

	var sb strings.Builder
	var ttft time.Duration
	for {
		chunk, err := stream.Recv()
		if err != nil {
			if !isEOF(err) {
				res.Error = err.Error()
			}
			break
		}
		if u := chunk.Usage; u != nil {
			res.InputTokens = u.InputTokens + u.CachedInputTokens + u.CacheWriteTokens
			res.OutputTokens = u.OutputTokens
			res.ReasoningTokens = u.ReasoningTokens
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				if ttft == 0 {
					ttft = time.Since(start)
				}
				sb.WriteString(choice.Delta.Content)
			}
		}
	}
	res.Summary = strings.TrimSpace(sb.String())
	res.TotalSeconds = time.Since(start).Seconds()
	res.TTFTSeconds = ttft.Seconds()
	res.CostUSD = float64(res.InputTokens)*spec.InPerTok + float64(res.OutputTokens)*spec.OutPerTok
	if res.Summary == "" && res.Error == "" {
		res.Error = "empty summary"
	}
	return res
}

func isEOF(err error) bool {
	return err != nil && (err.Error() == "EOF" || strings.Contains(err.Error(), "EOF"))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func boolPtr(b bool) *bool { return &b }

// injectReasoningEffort rewrites chat-completion request bodies to carry
// OpenRouter's `reasoning: {effort}` parameter.
func injectReasoningEffort(effort string) func(http.RoundTripper) http.RoundTripper {
	return func(base http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Body != nil && strings.Contains(req.URL.Path, "/chat/completions") {
				body, err := io.ReadAll(req.Body)
				req.Body.Close()
				if err != nil {
					return nil, err
				}
				var payload map[string]any
				if json.Unmarshal(body, &payload) == nil {
					payload["reasoning"] = map[string]any{"effort": effort}
					if b, err := json.Marshal(payload); err == nil {
						body = b
					}
				}
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.ContentLength = int64(len(body))
			}
			return base.RoundTrip(req)
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
