package anthropic

import (
	"slices"

	"github.com/docker/docker-agent/pkg/chat"
)

func (c *Client) betaSystemContext(messages []chat.Message) (sys []TextBlockParam, transient []string) {
	if !cachePreservingUpdatesEnabled(c.ModelConfig.ProviderOpts) {
		return extractSystemBlocks(messages), nil
	}
	for _, message := range slices.Backward(messages) {
		if message.Role == chat.MessageRoleSystem {
			continue
		}
		if message.Role == chat.MessageRoleAssistant {
			return extractSystemBlocks(messages), nil
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
	return extractSystemBlocks(stable), transient
}
