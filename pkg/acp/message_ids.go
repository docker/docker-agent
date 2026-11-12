package acp

import (
	"crypto/sha256"
	"encoding/json"
	"uuid"

	"github.com/coder/acp-go-sdk"
)

// Derive opaque UUIDv8 display IDs without exposing session or storage identifiers.
func messageDisplayID(owner, logical, channel string) string {
	key, _ := json.Marshal([]string{"docker-agent/acp-message/v1", owner, logical, channel})
	hash := sha256.Sum256(key)
	var id uuid.UUID
	copy(id[:], hash[:16])
	id[6] = id[6]&0x0f | 0x80
	id[8] = id[8]&0x3f | 0x80
	return id.String()
}

func withMessageID(update acp.SessionUpdate, id string) acp.SessionUpdate {
	switch {
	case update.UserMessageChunk != nil:
		update.UserMessageChunk.MessageId = &id
	case update.AgentMessageChunk != nil:
		update.AgentMessageChunk.MessageId = &id
	case update.AgentThoughtChunk != nil:
		update.AgentThoughtChunk.MessageId = &id
	}
	return update
}

func identifiedText(id string, update func(string) acp.SessionUpdate) func(string) acp.SessionUpdate {
	return func(text string) acp.SessionUpdate { return withMessageID(update(text), id) }
}

type liveMessageKey struct{ owner, agent, channel string }

type liveMessages struct {
	anonymous map[liveMessageKey]string
}

func (m *liveMessages) id(owner, agent, logical, channel string) string {
	if logical == "" {
		if m.anonymous == nil {
			m.anonymous = make(map[liveMessageKey]string)
		}
		key := liveMessageKey{owner, agent, channel}
		logical = m.anonymous[key]
		if logical == "" {
			logical = uuid.NewV4().String()
			m.anonymous[key] = logical
		}
	}
	return messageDisplayID(owner, logical, channel)
}

func (m *liveMessages) finish(owner, agent string) {
	delete(m.anonymous, liveMessageKey{owner, agent, "content"})
	delete(m.anonymous, liveMessageKey{owner, agent, "thought"})
}
