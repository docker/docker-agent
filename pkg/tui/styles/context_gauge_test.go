package styles

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestContextGaugeLevelFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		fill      float64
		threshold float64
		want      ContextGaugeLevel
	}{
		// Default threshold (0.9): warning at 0.675, critical at 0.855.
		{"empty context", 0, 0, ContextGaugeNormal},
		{"below warning band", 0.5, 0, ContextGaugeNormal},
		{"just below warning band", 0.674, 0, ContextGaugeNormal},
		{"warning band starts", 0.675, 0, ContextGaugeWarning},
		{"inside warning band", 0.80, 0, ContextGaugeWarning},
		{"critical band starts", 0.855, 0, ContextGaugeCritical},
		{"at threshold", 0.9, 0, ContextGaugeCritical},
		{"full context", 1.0, 0, ContextGaugeCritical},

		// Custom threshold scales the bands proportionally.
		{"custom threshold normal", 0.3, 0.5, ContextGaugeNormal},
		{"custom threshold warning", 0.4, 0.5, ContextGaugeWarning},
		{"custom threshold critical", 0.48, 0.5, ContextGaugeCritical},
		{"low fill high custom threshold", 0.7, 1.0, ContextGaugeNormal},

		// Out-of-range thresholds fall back to the default, mirroring
		// compaction.ShouldCompact.
		{"threshold above one falls back", 0.7, 1.5, ContextGaugeWarning},
		{"negative threshold falls back", 0.9, -1, ContextGaugeCritical},
		{"NaN threshold falls back", 0.7, math.NaN(), ContextGaugeWarning},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ContextGaugeLevelFor(tt.fill, tt.threshold))
		})
	}
}
