package tools

import "sync"

// DeferralTracker marks tools absent from a session's first model call as deferred.
type DeferralTracker struct {
	mu          sync.Mutex
	initial     map[string]map[string]struct{}
	loadPointBy map[string]map[string]string
}

func (t *DeferralTracker) Mark(sessionID string, requestTools []Tool) []Tool {
	return t.MarkAt(sessionID, "", requestTools)
}

func (t *DeferralTracker) MarkAt(sessionID, toolCallID string, requestTools []Tool) []Tool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.initial == nil {
		t.initial = make(map[string]map[string]struct{})
		t.loadPointBy = make(map[string]map[string]string)
	}
	initial, ok := t.initial[sessionID]
	if !ok {
		initial = make(map[string]struct{}, len(requestTools))
		for _, tool := range requestTools {
			if tool.SearchOnly {
				continue
			}
			initial[tool.Name] = struct{}{}
		}
		t.initial[sessionID] = initial
		t.loadPointBy[sessionID] = make(map[string]string)
		return requestTools
	}

	marked := make([]Tool, len(requestTools))
	copy(marked, requestTools)
	loadPoints := t.loadPointBy[sessionID]
	for i := range marked {
		if marked[i].SearchOnly {
			continue
		}
		if _, presentInitially := initial[marked[i].Name]; presentInitially {
			continue
		}
		if _, exists := loadPoints[marked[i].Name]; !exists {
			if toolCallID == "" {
				continue
			}
			loadPoints[marked[i].Name] = toolCallID
		}
		marked[i].Deferred = true
		marked[i].DeferredAtToolCallID = loadPoints[marked[i].Name]
	}
	return marked
}
