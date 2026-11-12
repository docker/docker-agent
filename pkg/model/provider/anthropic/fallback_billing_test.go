package anthropic

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic/anthropictest"
)

func TestFallbackBillingEvidenceAndHops(t *testing.T) {
	t.Parallel()
	const a, b, c = "claude-opus-4-8", "claude-opus-4-7", "claude-sonnet-4-6"
	zero := [][4]int64{{10, 0, 3, 4}}
	output := [][4]int64{{10, 7, 3, 4}}
	hop := func(model string, category any, tokens [][4]int64) anthropictest.BillingHop {
		return anthropictest.BillingHop{Model: model, Category: category, Iterations: tokens}
	}
	want := func(model string, billed, dispatched bool, tokens [4]int64) chat.UsageAttempt {
		return chat.UsageAttempt{
			Model: model, Billed: billed, Dispatched: dispatched,
			InputTokens: tokens[0], OutputTokens: tokens[1], CachedInputTokens: tokens[2], CacheWriteTokens: tokens[3],
		}
	}
	for name, tc := range map[string]struct {
		hops      []anthropictest.BillingHop
		exhausted bool
		mutate    func(string) string
		want      []chat.UsageAttempt
	}{
		"explicit null before output": {
			hops: []anthropictest.BillingHop{hop(a, nil, zero), hop(c, nil, output)},
			want: []chat.UsageAttempt{want(a, false, true, zero[0]), want(c, true, false, output[0])},
		},
		"missing category is billed": {
			hops: []anthropictest.BillingHop{{Model: a, MissingCategory: true, Iterations: zero}, hop(c, nil, output)},
			want: []chat.UsageAttempt{want(a, true, true, zero[0]), want(c, true, false, output[0])},
		},
		"empty category is billed": {
			hops: []anthropictest.BillingHop{hop(a, "", zero), hop(c, nil, output)},
			want: []chat.UsageAttempt{want(a, true, true, zero[0]), want(c, true, false, output[0])},
		},
		"invalid category is billed": {
			hops: []anthropictest.BillingHop{hop(a, 42, zero), hop(c, nil, output)},
			want: []chat.UsageAttempt{want(a, true, true, zero[0]), want(c, true, false, output[0])},
		},
		"exhausted chain terminal null": {
			hops: []anthropictest.BillingHop{hop(a, "cyber", zero), hop(b, "general_harms", zero), hop(c, nil, zero)}, exhausted: true,
			want: []chat.UsageAttempt{want(a, false, true, zero[0]), want(b, false, false, zero[0]), want(c, false, false, zero[0])},
		},
		"terminal missing category is billed": {
			hops: []anthropictest.BillingHop{hop(a, nil, zero), {Model: c, MissingCategory: true, Iterations: zero}}, exhausted: true,
			want: []chat.UsageAttempt{want(a, false, true, zero[0]), want(c, true, false, zero[0])},
		},
		"terminal missing details is billed": {
			hops: []anthropictest.BillingHop{hop(a, nil, zero), hop(c, nil, zero)}, exhausted: true,
			mutate: func(s string) string {
				return strings.ReplaceAll(s, `"stop_details":{"category":null,"type":"refusal"},`, "")
			},
			want: []chat.UsageAttempt{want(a, false, true, zero[0]), want(c, true, false, zero[0])},
		},
		"terminal null details is billed": {
			hops: []anthropictest.BillingHop{hop(a, nil, zero), hop(c, nil, zero)}, exhausted: true,
			mutate: func(s string) string {
				return strings.ReplaceAll(s, `"stop_details":{"category":null,"type":"refusal"}`, `"stop_details":null`)
			},
			want: []chat.UsageAttempt{want(a, false, true, zero[0]), want(c, true, false, zero[0])},
		},
		"wrong trigger type is unknown evidence": {
			hops: []anthropictest.BillingHop{hop(a, nil, zero), hop(c, nil, output)},
			mutate: func(s string) string {
				return strings.ReplaceAll(s, `"trigger":{"category":null,"type":"refusal"}`, `"trigger":{"category":null,"type":"other"}`)
			},
			want: []chat.UsageAttempt{want(a, true, true, zero[0]), want(c, true, false, output[0])},
		},
		"wrong stop details type is unknown evidence": {
			hops: []anthropictest.BillingHop{hop(a, nil, zero), hop(c, nil, zero)}, exhausted: true,
			mutate: func(s string) string {
				return strings.ReplaceAll(s, `"stop_details":{"category":null,"type":"refusal"}`, `"stop_details":{"category":null,"type":"other"}`)
			},
			want: []chat.UsageAttempt{want(a, false, true, zero[0]), want(c, true, false, zero[0])},
		},
		"successful empty fallback is billed": {
			hops: []anthropictest.BillingHop{hop(a, nil, zero), hop(c, nil, zero)},
			want: []chat.UsageAttempt{want(a, false, true, zero[0]), want(c, true, false, zero[0])},
		},
		"primary output before a zero output row still billed": {
			hops: []anthropictest.BillingHop{hop(a, nil, [][4]int64{{10, 7, 3, 4}, {20, 0, 5, 6}}), hop(c, nil, output)},
			want: []chat.UsageAttempt{want(a, true, true, [4]int64{30, 7, 8, 10}), want(c, true, false, output[0])},
		},
		"intermediate output before a zero output row still billed": {
			hops: []anthropictest.BillingHop{hop(a, nil, zero), hop(b, nil, [][4]int64{{10, 7, 3, 4}, {20, 0, 5, 6}}), hop(c, nil, output)},
			want: []chat.UsageAttempt{want(a, false, true, zero[0]), want(b, true, false, [4]int64{30, 7, 8, 10}), want(c, true, false, output[0])},
		},
		"exhausted terminal output in earlier row still billed": {
			hops: []anthropictest.BillingHop{hop(a, nil, zero), hop(c, nil, [][4]int64{{10, 7, 3, 4}, {20, 0, 5, 6}})}, exhausted: true,
			want: []chat.UsageAttempt{want(a, false, true, zero[0]), want(c, true, false, [4]int64{30, 7, 8, 10})},
		},
		"all zero output iterations exempt the whole hop": {
			hops: []anthropictest.BillingHop{hop(a, nil, [][4]int64{{10, 0, 3, 4}, {20, 0, 5, 6}}), hop(c, nil, [][4]int64{{10, 0, 3, 4}, {20, 0, 5, 6}})}, exhausted: true,
			want: []chat.UsageAttempt{want(a, false, true, [4]int64{30, 0, 8, 10}), want(c, false, false, [4]int64{30, 0, 8, 10})},
		},
		"all primary iterations retain dispatch identity": {
			hops: []anthropictest.BillingHop{hop(a, "bio", [][4]int64{{10, 0, 3, 4}, {20, 0, 5, 6}}), hop(c, nil, output)},
			want: []chat.UsageAttempt{want(a, true, true, [4]int64{30, 0, 8, 10}), want(c, true, false, output[0])},
		},
		"same count wrong marker model": {
			hops: []anthropictest.BillingHop{hop(a, nil, zero), hop(c, nil, zero)}, exhausted: true,
			mutate: func(s string) string {
				return strings.ReplaceAll(s, `"from":{"model":"`+a+`"}`, `"from":{"model":"wrong"}`)
			},
			want: []chat.UsageAttempt{want(a, true, false, zero[0]), want(c, true, false, zero[0])},
		},
		"same count wrong marker target": {
			hops: []anthropictest.BillingHop{hop(a, nil, zero), hop(c, nil, zero)}, exhausted: true,
			mutate: func(s string) string {
				return strings.ReplaceAll(s, `"to":{"model":"`+c+`"}`, `"to":{"model":"wrong"}`)
			},
			want: []chat.UsageAttempt{want(a, true, true, zero[0]), want(c, true, true, zero[0])},
		},
		"marker count mismatch remains billed": {
			hops: []anthropictest.BillingHop{hop(a, nil, zero), hop(b, nil, zero), hop(c, nil, zero)}, exhausted: true,
			mutate: func(s string) string {
				start := strings.Index(s, "event: content_block_start")
				end := strings.Index(s, "event: content_block_stop")
				end += strings.Index(s[end:], "\n\n") + 2
				return s[:start] + s[end:]
			},
			want: []chat.UsageAttempt{want(a, true, false, zero[0]), want(b, true, false, zero[0]), want(c, true, false, zero[0])},
		},
		"other iteration types do not create model hops": {
			hops: []anthropictest.BillingHop{hop(a, nil, zero), hop(c, nil, output)},
			mutate: func(s string) string {
				return strings.ReplaceAll(s, `"iterations":[`, `"iterations":[{"type":"compaction","model":"other","input_tokens":123,"output_tokens":9},`)
			},
			want: []chat.UsageAttempt{want(a, false, true, zero[0]), want(c, true, false, output[0])},
		},
		"alias from echo aligns with dated usage model": {
			hops: []anthropictest.BillingHop{{Model: a + "-20260901", From: a, Iterations: zero}, hop(c, nil, output)},
			want: []chat.UsageAttempt{want(a+"-20260901", false, true, zero[0]), want(c, true, false, output[0])},
		},
		"sticky tool loop is not dispatched": {
			hops: []anthropictest.BillingHop{hop(c, nil, [][4]int64{{10, 0, 3, 4}, {20, 7, 5, 6}})},
			want: []chat.UsageAttempt{want(c, true, false, [4]int64{30, 7, 8, 10})},
		},
		"sticky model later declines": {
			hops: []anthropictest.BillingHop{hop(b, nil, zero), hop(c, nil, output)},
			want: []chat.UsageAttempt{want(b, false, false, zero[0]), want(c, true, false, output[0])},
		},
		"three hop mixed refusal evidence": {
			hops: []anthropictest.BillingHop{hop(a, nil, zero), hop(b, "bio", zero), hop(c, nil, output)},
			want: []chat.UsageAttempt{want(a, false, true, zero[0]), want(b, true, false, zero[0]), want(c, true, false, output[0])},
		},
		"misaligned runs retain only primary prefix dispatch": {
			hops: []anthropictest.BillingHop{hop(a, nil, zero), hop(c, nil, output)},
			mutate: func(s string) string {
				return strings.ReplaceAll(s, `"iterations":[`, `"iterations":[{"type":"message","model":"unexpected","input_tokens":1,"output_tokens":0},`)
			},
			want: []chat.UsageAttempt{want("unexpected", true, true, [4]int64{1, 0, 0, 0}), want(a, true, true, zero[0]), want(c, true, false, output[0])},
		},
		"repeated nonadjacent model is a separate hop": {
			hops: []anthropictest.BillingHop{hop(a, nil, zero), hop(b, nil, zero), hop(a, nil, output)},
			want: []chat.UsageAttempt{want(a, false, true, zero[0]), want(b, false, false, zero[0]), want(a, true, false, output[0])},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reply := anthropictest.FallbackBillingReply("m", tc.exhausted, tc.hops...)
			if tc.mutate != nil {
				mutated := tc.mutate(reply)
				require.NotEqual(t, reply, mutated, "mutation must exercise malformed evidence")
				reply = mutated
			}
			stream, err := fallbackClient(newScriptedServer(t, reply)).CreateChatCompletionStream(t.Context(), []chat.Message{user("q")}, nil)
			require.NoError(t, err)
			defer stream.Close()
			assert.Equal(t, tc.want, finalAttempts(t, stream))
		})
	}
}

func TestFallbackBillingCategories(t *testing.T) {
	t.Parallel()
	for _, category := range []any{nil, "", 42, "cyber", "general_harms", "bio", "frontier_llm", "reasoning_extraction"} {
		for _, terminal := range []bool{false, true} {
			name := "marker"
			if terminal {
				name = "terminal"
			}
			t.Run(name+"/"+stringCategory(category), func(t *testing.T) {
				t.Parallel()
				hops := []anthropictest.BillingHop{
					{Model: "claude-opus-4-8", Category: category, Iterations: [][4]int64{{10, 0, 3, 4}}},
					{Model: "claude-sonnet-4-6", Category: category, Iterations: [][4]int64{{10, 0, 3, 4}}},
				}
				stream, err := fallbackClient(newScriptedServer(t, anthropictest.FallbackBillingReply("m", terminal, hops...))).CreateChatCompletionStream(t.Context(), []chat.Message{user("q")}, nil)
				require.NoError(t, err)
				defer stream.Close()
				attempts := finalAttempts(t, stream)
				require.Len(t, attempts, 2)
				billed := category == "" || category == 42 || category == "bio" || category == "frontier_llm" || category == "reasoning_extraction"
				assert.Equal(t, billed, attempts[0].Billed)
				assert.Equal(t, !terminal || billed, attempts[1].Billed)
			})
		}
	}
}

func TestFallbackBeforeOutputNullCategory(t *testing.T) {
	t.Parallel()
	reply := anthropictest.FallbackBeforeOutputReply("m", "claude-opus-4-8", "claude-sonnet-4-6", "", "hi")
	require.Contains(t, reply, `"category":null`)
	stream, err := fallbackClient(newScriptedServer(t, reply)).CreateChatCompletionStream(t.Context(), []chat.Message{user("q")}, nil)
	require.NoError(t, err)
	defer stream.Close()
	attempts := finalAttempts(t, stream)
	require.Len(t, attempts, 2)
	assert.False(t, attempts[0].Billed)
	assert.True(t, attempts[1].Billed)
}

func stringCategory(category any) string {
	if category == nil {
		return "null"
	}
	return fmt.Sprint(category)
}
