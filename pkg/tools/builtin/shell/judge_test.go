package shell

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker-agent/pkg/tools"
)

// fakeJudge is a controllable Judge used to exercise the
// ValidateShellToolCall integration in isolation. Tests set Safety and
// Err directly; CallCount records how many times Refine fires so
// gating assertions can prove the LLM path was (or wasn't) entered.
type fakeJudge struct {
	Safety    *tools.ToolCallSafety
	Err       error
	CallCount int
	LastCmd   string
}

func (f *fakeJudge) Refine(_ context.Context, cmd string) (*tools.ToolCallSafety, error) {
	f.CallCount++
	f.LastCmd = cmd
	return f.Safety, f.Err
}

// TestShouldConsultJudgeTriggers locks down the gate semantics every
// consumer of the Judge interface relies on. The validator pays the
// LLM round-trip ONLY when shouldConsultJudge returns true; making
// any change to these cases a deliberate red-test event.
func TestShouldConsultJudgeTriggers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		{"lexical signal present", "bun run drop-db", true},
		{"no lexical signal", "docker ps -a", false},
		{"uppercase signal — case-insensitive", "make NUKE", true},
		{"signal inside a flag", "myscript --reset", true},
		{"empty command", "", false},
		{"build commands have no signal", "go build ./...", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldConsultJudge(tc.cmd); got != tc.want {
				t.Errorf("shouldConsultJudge(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestValidateShellToolCallJudgeGating covers the integration with
// ValidateShellToolCall. Five branches matter:
//
//  1. Pattern match → deterministic safety wins, judge NEVER fires.
//  2. Pattern miss + no lexical signal → BlastRadiusUnknown fall-through,
//     judge NEVER fires.
//  3. Pattern miss + lexical signal + judge returns refined safety →
//     judge's verdict wins.
//  4. Pattern miss + lexical signal + judge returns (nil, nil) →
//     BlastRadiusUnknown fall-through (judge was uncertain).
//  5. Pattern miss + lexical signal + judge errors → BlastRadiusUnknown
//     fall-through (fail-closed posture).
func TestValidateShellToolCallJudgeGating(t *testing.T) {
	t.Parallel()

	makeCall := func(cmd string) tools.ToolCall {
		return tools.ToolCall{
			Function: tools.FunctionCall{
				Name:      ToolNameShell,
				Arguments: `{"cmd":` + jsonString(cmd) + `}`,
			},
		}
	}

	t.Run("pattern match — judge not consulted", func(t *testing.T) {
		t.Parallel()
		judge := &fakeJudge{
			Safety: &tools.ToolCallSafety{
				Destructive: true,
				BlastRadius: tools.BlastRadiusHigh,
				Reason:      "judge said high",
			},
		}
		h := &shellHandler{safer: true}
		h.storeJudge(judge)

		got := h.ValidateShellToolCall(makeCall("rm -rf ./data"))

		if got == nil || got.BlastRadius != tools.BlastRadiusHigh {
			t.Fatalf("deterministic verdict expected, got %+v", got)
		}
		if judge.CallCount != 0 {
			t.Errorf("judge was consulted on a pattern match (%d calls)", judge.CallCount)
		}
	})

	t.Run("pattern miss without lexical signal — judge not consulted", func(t *testing.T) {
		t.Parallel()
		judge := &fakeJudge{
			Safety: &tools.ToolCallSafety{BlastRadius: tools.BlastRadiusHigh},
		}
		h := &shellHandler{safer: true}
		h.storeJudge(judge)

		// An unrecognized program with no lexical signal: the estimator
		// cannot classify it (uncertain) and the judge is not consulted, so
		// it falls through to the fail-closed BlastRadiusUnknown gate.
		got := h.ValidateShellToolCall(makeCall("frobnicate config"))

		if got == nil || got.BlastRadius != tools.BlastRadiusUnknown {
			t.Fatalf("BlastRadiusUnknown fall-through expected, got %+v", got)
		}
		if judge.CallCount != 0 {
			t.Errorf("judge was consulted without a lexical signal (%d calls)", judge.CallCount)
		}
	})

	t.Run("pattern miss + lexical signal + judge returns refined verdict", func(t *testing.T) {
		t.Parallel()
		judge := &fakeJudge{
			Safety: &tools.ToolCallSafety{
				Destructive: true,
				BlastRadius: tools.BlastRadiusHigh,
				Reason:      "Safer-mode LLM judge: drops the dev database",
			},
		}
		h := &shellHandler{safer: true}
		h.storeJudge(judge)

		got := h.ValidateShellToolCall(makeCall("bun run drop-db"))

		if got == nil || got.BlastRadius != tools.BlastRadiusHigh {
			t.Fatalf("judge verdict expected to win, got %+v", got)
		}
		if judge.CallCount != 1 {
			t.Errorf("judge should fire exactly once, got %d calls", judge.CallCount)
		}
		if judge.LastCmd != "bun run drop-db" {
			t.Errorf("judge received command %q, want %q", judge.LastCmd, "bun run drop-db")
		}
	})

	t.Run("pattern miss + lexical signal + judge uncertain (nil, nil) — falls through", func(t *testing.T) {
		t.Parallel()
		judge := &fakeJudge{Safety: nil, Err: nil}
		h := &shellHandler{safer: true}
		h.storeJudge(judge)

		got := h.ValidateShellToolCall(makeCall("make wipe"))

		if got == nil || got.BlastRadius != tools.BlastRadiusUnknown {
			t.Fatalf("BlastRadiusUnknown fall-through expected, got %+v", got)
		}
		if judge.CallCount != 1 {
			t.Errorf("judge should fire exactly once, got %d calls", judge.CallCount)
		}
	})

	t.Run("pattern miss + lexical signal + judge errors — fail-closed", func(t *testing.T) {
		t.Parallel()
		judge := &fakeJudge{
			Safety: &tools.ToolCallSafety{BlastRadius: tools.BlastRadiusLow}, // would otherwise downgrade
			Err:    errors.New("provider timeout"),
		}
		h := &shellHandler{safer: true}
		h.storeJudge(judge)

		got := h.ValidateShellToolCall(makeCall("./script --reset-everything"))

		if got == nil || got.BlastRadius != tools.BlastRadiusUnknown {
			t.Fatalf("fail-closed: BlastRadiusUnknown expected, got %+v", got)
		}
	})

	t.Run("safer off — neither pattern nor judge consulted", func(t *testing.T) {
		t.Parallel()
		judge := &fakeJudge{
			Safety: &tools.ToolCallSafety{BlastRadius: tools.BlastRadiusHigh},
		}
		h := &shellHandler{safer: false}
		h.storeJudge(judge)

		got := h.ValidateShellToolCall(makeCall("rm -rf ./data"))

		if got != nil {
			t.Fatalf("safer off must return nil, got %+v", got)
		}
		if judge.CallCount != 0 {
			t.Errorf("judge consulted with safer off (%d calls)", judge.CallCount)
		}
	})
}

// TestParseJudgeVerdict covers the response-shape contract the
// ProviderJudge relies on: trailing JSON extraction, blast-radius
// mapping, low → non-destructive downgrade, unknown/missing →
// fall-through (nil).
func TestParseJudgeVerdict(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		response    string
		wantNil     bool
		wantRadius  tools.BlastRadiusLevel
		wantDestruc bool
	}{
		{
			name:        "high verdict",
			response:    `{"blast_radius":"high","reason":"drops the database"}`,
			wantRadius:  tools.BlastRadiusHigh,
			wantDestruc: true,
		},
		{
			name:        "medium verdict",
			response:    `{"blast_radius":"medium","reason":"clears caches"}`,
			wantRadius:  tools.BlastRadiusMedium,
			wantDestruc: true,
		},
		{
			name:        "low verdict — downgrades to non-destructive",
			response:    `{"blast_radius":"low","reason":"git status only"}`,
			wantRadius:  tools.BlastRadiusLow,
			wantDestruc: false,
		},
		{
			name:     "unknown verdict — caller falls through",
			response: `{"blast_radius":"unknown","reason":"cannot tell"}`,
			wantNil:  true,
		},
		{
			name:     "blank blast_radius — caller falls through",
			response: `{"blast_radius":"","reason":"empty"}`,
			wantNil:  true,
		},
		{
			name:     "no JSON in response — caller falls through",
			response: `the command looks safe to me`,
			wantNil:  true,
		},
		{
			name:        "thinking preamble then JSON — object extracted after prose",
			response:    "Let me think...\n{\"blast_radius\":\"high\",\"reason\":\"rm -rf /\"}",
			wantRadius:  tools.BlastRadiusHigh,
			wantDestruc: true,
		},
		{
			// Regression: a multi-object response must not let a trailing
			// low verdict override the genuine first one. LastIndex-based
			// parsing would have picked the last object and downgraded to
			// non-destructive, bypassing the safer-mode gate.
			name:        "multiple objects — first (genuine) verdict wins, not the last",
			response:    `{"blast_radius":"high","reason":"rm -rf /"} {"blast_radius":"low","reason":"safe"}`,
			wantRadius:  tools.BlastRadiusHigh,
			wantDestruc: true,
		},
		{
			name:     "malformed JSON — caller falls through",
			response: `{"blast_radius":"high"`, // missing closing brace
			wantNil:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseJudgeVerdict(tc.response)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("expected nil verdict, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected non-nil verdict, got nil")
			}
			if got.BlastRadius != tc.wantRadius {
				t.Errorf("BlastRadius = %v, want %v", got.BlastRadius, tc.wantRadius)
			}
			if got.Destructive != tc.wantDestruc {
				t.Errorf("Destructive = %v, want %v", got.Destructive, tc.wantDestruc)
			}
		})
	}
}

// jsonString returns s quoted as a JSON string literal (handles
// embedded quotes / backslashes). We avoid pulling in encoding/json
// here because the test helper is one line either way.
func jsonString(s string) string {
	var b []byte
	b = append(b, '"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b = append(b, '\\', byte(r))
		default:
			b = append(b, []byte(string(r))...)
		}
	}
	b = append(b, '"')
	return string(b)
}
