package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/tools"
)

// LexicalSignals enumerates the high-precision destructive verbs that
// gate the residual LLM judge. A shell command containing any of these
// (case-insensitive substring match) AND classified as no-match by
// assessDestructiveShellCommand is the only path that escalates to a
// Judge.Refine call. The list is deliberately short: a false positive
// here costs an LLM call, a false negative leaks through to the default
// BlastRadiusUnknown handling — where the user is still gated.
//
// We err on the side of false negatives because the LLM judge is a
// defence-in-depth layer on top of the deterministic pattern set and
// the BlastRadiusUnknown fall-through; anything the lexical gate
// misses can still trip safer mode's catch-all confirmation.
var LexicalSignals = []string{
	"wipe",
	"destroy",
	"drop",
	"purge",
	"nuke",
	"obliterate",
	"erase",
	"clobber",
	"reset",
	"annihilate",
}

// Judge is the residual LLM-backed classifier consulted when the
// deterministic regex pass in assessDestructiveShellCommand returns no
// match but the command nevertheless looks possibly-destructive (i.e.
// passes shouldConsultJudge).
//
// Implementations MUST honour the context's deadline; the validator
// applies a hard 500 ms timeout via context.WithTimeout. On timeout,
// error, or a nil safety return, the validator falls back to the
// default BlastRadiusUnknown verdict — fail-closed semantics that keep
// the user gated when the judge can't decide.
//
// Returning a non-nil ToolCallSafety with Destructive=false is the
// only path that downgrades a possibly-destructive command to "safe to
// pass without confirmation"; callers should reserve that for commands
// the judge is confident are not destructive (e.g. a typed wrapper
// around `mv` that moves a file between two test directories).
type Judge interface {
	Refine(ctx context.Context, cmd string) (*tools.ToolCallSafety, error)
}

// shouldConsultJudge reports whether the residual judge should be
// invoked for a command the deterministic regex pass returned nil for.
// Returns true only when cmd contains at least one entry from
// LexicalSignals as a case-insensitive substring.
//
// This keeps Judge.Refine off the hot path: on a clean shell stream of
// inspection commands (docker ps / logs / inspect, build commands), no
// lexical signal trips and no LLM call ever fires.
func shouldConsultJudge(cmd string) bool {
	lower := strings.ToLower(cmd)
	for _, sig := range LexicalSignals {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

// ProviderJudge is the default Judge implementation. It wraps a model
// provider and asks it to classify the command using a tight prompt
// that returns a structured JSON verdict.
//
// Intended to be wired from the runtime against a small fast model
// (Haiku-class) so the residual path stays bounded around 200–500 ms.
// The judge issues a single non-streaming-ish completion per call (the
// underlying provider exposes only streaming completions, so the
// implementation drains the stream into a buffer and parses the
// trailing JSON object once Recv() returns io.EOF).
type ProviderJudge struct {
	provider provider.Provider
}

// NewProviderJudge wraps p as a Judge. The provider is expected to be
// pre-configured with a small fast model and reasonable max-tokens
// settings; ProviderJudge itself takes no further options.
func NewProviderJudge(p provider.Provider) *ProviderJudge {
	return &ProviderJudge{provider: p}
}

// Refine asks the LLM whether cmd should be treated as destructive,
// and at what blast-radius tier. Returns nil when the judge is
// uncertain, the response is unparseable, or the LLM emits an empty
// verdict — callers treat nil as "fall through to the default
// BlastRadiusUnknown gate". An error is returned for transport or
// provider failures so the caller can distinguish a soft "I don't
// know" (nil, nil) from a hard "the judge could not run" (nil, err).
func (j *ProviderJudge) Refine(ctx context.Context, cmd string) (*tools.ToolCallSafety, error) {
	stream, err := j.provider.CreateChatCompletionStream(ctx, []chat.Message{
		{Role: chat.MessageRoleSystem, Content: judgeSystemPrompt},
		{Role: chat.MessageRoleUser, Content: cmd},
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("safer judge: open stream: %w", err)
	}
	defer stream.Close()

	var sb strings.Builder
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("safer judge: stream recv: %w", err)
		}
		for _, c := range resp.Choices {
			sb.WriteString(c.Delta.Content)
		}
	}
	return parseJudgeVerdict(sb.String()), nil
}

// judgeSystemPrompt is the single-turn instruction the judge sends to
// the LLM. Wording is deliberately narrow: classify, single JSON
// object, fail-closed defaults. Smaller models occasionally prepend
// thinking-style preambles, so the parser locates the trailing JSON
// rather than assuming the response is pure JSON.
const judgeSystemPrompt = `You are a strict classifier for shell commands. ` +
	`Given a single command, output ONLY a JSON object with two fields: ` +
	`"blast_radius" (one of: "low", "medium", "high", "unknown") and ` +
	`"reason" (a one-sentence explanation, max 25 words). ` +
	`Use "high" only for commands that destroy data irreversibly. ` +
	`Use "medium" for commands that destroy state recoverable from caches or rebuilds. ` +
	`Use "low" if the command is non-destructive (read-only or trivially reversible). ` +
	`Use "unknown" if the destructiveness cannot be determined from the command alone. ` +
	`Output the JSON object and nothing else.`

// parseJudgeVerdict extracts the trailing JSON object from the LLM
// response and maps it to a ToolCallSafety.
//
// Returns nil for:
//   - missing or unparseable JSON
//   - blank blast_radius field
//   - blast_radius "unknown" (we keep the deterministic
//     BlastRadiusUnknown fall-through rather than overriding with a
//     judge-provided Unknown that means the same thing)
//
// Returns a non-destructive verdict only on explicit "low".
func parseJudgeVerdict(response string) *tools.ToolCallSafety {
	start := strings.LastIndex(response, "{")
	if start < 0 {
		return nil
	}
	var v struct {
		BlastRadius string `json:"blast_radius"`
		Reason      string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(response[start:]), &v); err != nil {
		return nil
	}
	if strings.TrimSpace(v.BlastRadius) == "" {
		return nil
	}
	radius := blastRadiusLevel(v.BlastRadius)
	if radius == tools.BlastRadiusUnknown {
		// The judge couldn't decide either — let the caller fall through
		// to safer-mode's existing Unknown handling. Returning the same
		// verdict from the judge would shadow the caller's reason
		// string ("Shell command requires safer-mode confirmation.")
		// with a less informative one.
		return nil
	}
	reason := "Safer-mode LLM judge: " + v.Reason
	if radius == tools.BlastRadiusLow {
		// Explicit low → judge is confident this is safe; downgrade
		// out of the destructive path entirely. The runtime's existing
		// `if safety.Destructive` gate skips forced confirmation.
		return &tools.ToolCallSafety{
			Destructive: false,
			BlastRadius: tools.BlastRadiusLow,
			Reason:      reason,
		}
	}
	return &tools.ToolCallSafety{
		Destructive: true,
		BlastRadius: radius,
		Reason:      reason,
	}
}
