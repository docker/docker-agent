package root

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestRunExecFlagsApplyUserSettingsLean(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		lean        bool
		leanChanged bool
		wantLean    bool
	}{
		{
			name:     "applies lean default",
			wantLean: true,
		},
		{
			name:        "keeps explicit lean false",
			leanChanged: true,
			wantLean:    false,
		},
		{
			name:        "keeps explicit lean true",
			lean:        true,
			leanChanged: true,
			wantLean:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			flags := &runExecFlags{
				lean:        tt.lean,
				leanChanged: tt.leanChanged,
			}
			flags.applyUserSettings(t.Context(), &userconfig.Settings{Lean: true})

			assert.Equal(t, tt.wantLean, flags.lean)
		})
	}
}
