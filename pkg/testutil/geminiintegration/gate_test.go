package geminiintegration

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSkipReason(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name          string
		githubActions string
		optIn         string
		want          string
	}{
		{name: "default off", want: "require " + OptInEnv + "=1"},
		{name: "explicit reviewer opt-in", optIn: "1"},
		{name: "zero is not opt-in", optIn: "0", want: "require " + OptInEnv + "=1"},
		{name: "true is not opt-in", optIn: "true", want: "require " + OptInEnv + "=1"},
		{name: "whitespace is not opt-in", optIn: " 1", want: "require " + OptInEnv + "=1"},
		{name: "actions without opt-in", githubActions: "true", want: "disabled in GitHub Actions"},
		{name: "actions overrides opt-in", githubActions: "true", optIn: "1", want: "disabled in GitHub Actions"},
		{name: "nonempty actions is fail closed", githubActions: "false", optIn: "1", want: "disabled in GitHub Actions"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason := skipReason(tt.githubActions, tt.optIn)
			if tt.want == "" {
				assert.Empty(t, reason)
			} else {
				assert.Contains(t, reason, tt.want)
			}
		})
	}
}
