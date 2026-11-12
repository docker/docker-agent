package evaluation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestLLMJudgeBackend(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name, response, reason string
		passed, wantErr        bool
	}{
		{"pass", `{"result":"pass","reason":"satisfied"}`, "satisfied", true, false},
		{"fail", `{"result":"fail","reason":"not satisfied"}`, "not satisfied", false, false},
		{"invalid JSON", "not JSON", "", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			model := &replayProvider{streams: [][]chat.MessageStreamResponse{{{
				Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: tt.response}}},
			}}}}
			judge := NewJudge(model, 1)
			results, err := judge.CheckRelevance(t.Context(), "transcript", []string{"criterion"})
			if tt.wantErr {
				require.ErrorContains(t, err, "parsing judge response")
				return
			}
			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, tt.passed, results[0].Passed)
			assert.Equal(t, tt.reason, results[0].Reason)
			assert.Nil(t, results[0].Probability)
		})
	}
}
