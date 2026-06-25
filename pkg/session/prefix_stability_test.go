package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
)

// cacheablePrefix returns the messages that form the cacheable prefix: all
// messages up to and including the second cache-control checkpoint.  These
// correspond to the Anthropic "system" parameter blocks that lie before
// (a) the invariant checkpoint (CP1) and (b) the extras checkpoint (CP2).
// Both must be byte-stable across consecutive turns for prompt-cache reads
// to hit.  Returns nil when fewer than two checkpoints are present.
func cacheablePrefix(messages []chat.Message) []chat.Message {
	cp := 0
	end := -1
	for i, msg := range messages {
		if msg.Role == chat.MessageRoleSystem && msg.CacheControl {
			cp++
			if cp == 2 {
				end = i
				break
			}
		}
	}
	if end < 0 {
		return nil
	}
	return messages[:end+1]
}

// TestPrefixStabilityAcrossConversationGrowth is the prefix-stability
// regression guard:
//
//   - The cacheable prefix (system messages up to and including CP2) must be
//     byte-identical across two consecutive GetMessages calls whose only
//     difference is additional conversation history.
//
// Any future change that accidentally injects a timestamp, per-request ID,
// counter, or other volatile value into the invariant system messages or the
// turn_start / session_start extras will break this test, alerting the
// author that prompt-cache efficiency would be degraded for every turn.
func TestPrefixStabilityAcrossConversationGrowth(t *testing.T) {
	testAgent := agent.New("root", "test agent instructions",
		agent.WithToolSets(todoToolSet(t)),
	)

	// Extras represent what the runtime passes on each turn:
	// session_start env info (stable) + turn_start date (stable within a day).
	sessionStart := chat.Message{
		Role:    chat.MessageRoleSystem,
		Content: "Here is useful information about the environment: Working directory: /workspace",
	}
	turnStart := chat.Message{
		Role:    chat.MessageRoleSystem,
		Content: "Today's date: 2026-06-25",
	}
	extras := []chat.Message{sessionStart, turnStart}

	// — Turn N: session has two messages ————————————————————————————————
	sess := New()
	sess.AddMessage(NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleUser, Content: "Hello!"}))
	sess.AddMessage(NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "Hi there!"}))

	msgsN := sess.GetMessages(testAgent, extras...)
	prefixN := cacheablePrefix(msgsN)
	require.NotNil(t, prefixN, "expected two cache-control checkpoints in turn N")

	// — Turn N+1: history grew by one more round-trip ————————————————————
	sess.AddMessage(NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleUser, Content: "What can you do?"}))
	sess.AddMessage(NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "I can help with tasks!"}))

	msgsN1 := sess.GetMessages(testAgent, extras...)
	prefixN1 := cacheablePrefix(msgsN1)
	require.NotNil(t, prefixN1, "expected two cache-control checkpoints in turn N+1")

	// The prefixes must be byte-identical.
	assert.Equal(t, prefixN, prefixN1,
		"cacheable prefix (system messages up to CP2) must be byte-stable "+
			"across consecutive turns: any difference busts prompt-cache reads "+
			"for every turn of every session")
}

// TestPrefixStabilityIdempotent asserts that calling GetMessages twice in
// succession with identical inputs produces identical prefixes.  This guards
// against non-deterministic sources inside GetMessages itself (e.g.
// map-iteration order, pointer-dependent formatting).
func TestPrefixStabilityIdempotent(t *testing.T) {
	testAgent := agent.New("root", "determinism test",
		agent.WithToolSets(todoToolSet(t)),
	)

	extra := chat.Message{
		Role:    chat.MessageRoleSystem,
		Content: "Today's date: 2026-06-25",
	}

	sess := New()
	sess.AddMessage(NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleUser, Content: "hello"}))

	prefix1 := cacheablePrefix(sess.GetMessages(testAgent, extra))
	prefix2 := cacheablePrefix(sess.GetMessages(testAgent, extra))

	require.NotNil(t, prefix1)
	assert.Equal(t, prefix1, prefix2,
		"repeated GetMessages calls with identical inputs must yield identical prefixes")
}
