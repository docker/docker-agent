package anthropic

import (
	"slices"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/docker/docker-agent/pkg/chat"
)

// betaSystemContext splits the system messages of a Beta request into the
// stable top-level system blocks and the turn-scoped reminder texts that
// applyConversationUpdates appends after the last user turn with
// clear_at: next_user_message. Without cache_preserving_updates every
// system message is a top-level block, exactly as before.
//
// Only system messages are ever turn-scoped: the flag is ignored on user
// and tool messages so untrusted content is never promoted to a system
// reminder. Text extraction (trimming, one block per multi-content part)
// is shared with the top-level path so both render identically.
func (c *Client) betaSystemContext(messages []chat.Message) (sys []anthropic.BetaTextBlockParam, transient []string) {
	if !cachePreservingUpdatesEnabled(c.ModelConfig.ProviderOpts) {
		return extractBetaSystemBlocks(messages), nil
	}
	// A structured-output retry can end on an assistant turn. Keep reminders
	// top-level there; resetting the cache is preferable to an invalid placement.
	for _, message := range slices.Backward(messages) {
		if message.Role == chat.MessageRoleSystem {
			continue
		}
		if message.Role == chat.MessageRoleAssistant {
			return extractBetaSystemBlocks(messages), nil
		}
		break
	}
	var stable, scoped []chat.Message
	for i := range messages {
		msg := &messages[i]
		if msg.Role != chat.MessageRoleSystem {
			continue
		}
		if msg.TurnScoped {
			scoped = append(scoped, *msg)
		} else {
			stable = append(stable, *msg)
		}
	}
	for _, block := range extractSystemBlocks(scoped) {
		transient = append(transient, block.Text)
	}
	return extractBetaSystemBlocks(stable), transient
}
