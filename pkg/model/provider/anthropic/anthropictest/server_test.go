package anthropictest_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/model/provider/anthropic/anthropictest"
)

func TestServerRepliesAndRecords(t *testing.T) {
	t.Parallel()

	srv := anthropictest.NewServer(t, anthropictest.TextReply("msg_1", "hi", 42))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL()+"/v1/messages", strings.NewReader(`{"model":"m"}`))
	require.NoError(t, err)
	req.Header.Set("anthropic-beta", "a, b")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Contains(t, string(body), `"input_tokens":42`)
	assert.Contains(t, string(body), "event: message_stop")
	got := srv.Requests()
	require.Len(t, got, 1)
	assert.Equal(t, []string{"a", "b"}, got[0].Betas)
	assert.Equal(t, "m", got[0].Body["model"])
}

// The fallback replies must parse with the SDK, which relabels the message
// with the model that served it.
func TestFallbackRepliesAccumulate(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		reply   string
		markers int
	}{
		"before output": {reply: anthropictest.FallbackBeforeOutputReply("m1", "claude-a", "claude-b", "cyber", "hi"), markers: 1},
		"mid output":    {reply: anthropictest.FallbackMidOutputReply("m2", "claude-a", "claude-b", "before", "after", "t1"), markers: 1},
		"sticky":        {reply: anthropictest.StickyReply("m3", "claude-b", "hi")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := anthropictest.NewServer(t, tc.reply)
			client := anthropic.NewClient(option.WithBaseURL(srv.URL()), option.WithAPIKey("k"))
			stream := client.Beta.Messages.NewStreaming(t.Context(), anthropic.BetaMessageNewParams{
				Model: "claude-a", MaxTokens: 16,
				Messages: []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("q"))},
			})
			var msg anthropic.BetaMessage
			for stream.Next() {
				require.NoError(t, msg.Accumulate(stream.Current()))
			}
			require.NoError(t, stream.Err())

			assert.Equal(t, anthropic.Model("claude-b"), msg.Model)
			markers := 0
			for _, block := range msg.Content {
				if block.Type == "fallback" {
					markers++
				}
			}
			assert.Equal(t, tc.markers, markers)
			assert.NotEmpty(t, msg.Usage.Iterations)
		})
	}
}
