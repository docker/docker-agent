package chat

import "github.com/docker/docker-agent/pkg/tui/styles"

type chatPaneCache struct {
	content       string
	width, height int
	rendered      string
	valid         bool
}

// Cache only formatting: messagesView still runs each frame to update scroll state.
func (p *chatPage) renderChatPane(content string, width, height int) string {
	c := &p.chatPaneCache
	if c.valid && c.width == width && c.height == height && c.content == content {
		return c.rendered
	}
	*c = chatPaneCache{
		content:  content,
		width:    width,
		height:   height,
		rendered: styles.ChatStyle.Height(height).Width(width).Render(content),
		valid:    true,
	}
	return c.rendered
}
