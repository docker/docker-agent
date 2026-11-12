// Package streamcontent tracks logical assistant-message boundaries.
package streamcontent

import "strconv"

type Identity struct {
	SessionID string
	MessageID string
}

// Tracker gives legacy events without message IDs a per-message identity.
type Tracker struct {
	sequence uint64
	legacy   map[string]string
}

func (t *Tracker) Resolve(sessionID, messageID string) Identity {
	if messageID == "" {
		if t.legacy == nil {
			t.legacy = make(map[string]string)
		}
		messageID = t.legacy[sessionID]
		if messageID == "" {
			t.sequence++
			messageID = "legacy:" + strconv.FormatUint(t.sequence, 10)
			t.legacy[sessionID] = messageID
		}
	}
	return Identity{SessionID: sessionID, MessageID: messageID}
}

func (t *Tracker) Finish(sessionID string) { delete(t.legacy, sessionID) }
