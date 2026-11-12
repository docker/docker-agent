// Package geminiintegration gates reviewer-only Gemini integration fixtures.
package geminiintegration

import "os"

// OptInEnv enables local reviewer fixtures only when set to exactly "1".
// Any nonempty GITHUB_ACTIONS value overrides the opt-in, including "false".
const OptInEnv = "DOCKER_AGENT_RUN_GEMINI_INTEGRATION_TESTS"

// TB is the test interface needed by Require.
type TB interface {
	Helper()
	Skip(args ...any)
}

// Require skips before fixture setup unless a local reviewer explicitly opts in.
// Opting in does not authorize live requests or cassette recording.
func Require(t TB) {
	t.Helper()
	if reason := skipReason(os.Getenv("GITHUB_ACTIONS"), os.Getenv(OptInEnv)); reason != "" {
		t.Skip(reason)
	}
}

func skipReason(githubActions, optIn string) string {
	if githubActions != "" {
		return "Gemini integration fixtures are disabled in GitHub Actions regardless of " + OptInEnv
	}
	if optIn != "1" {
		return "reviewer-only Gemini integration fixtures require " + OptInEnv + "=1"
	}
	return ""
}
