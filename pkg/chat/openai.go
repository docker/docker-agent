package chat

import "encoding/json"

// OpenAIResponse preserves ordered Responses output for stateless continuation.
// Source scopes opaque state to the originating provider and model.
type OpenAIResponse struct {
	ID        string            `json:"id,omitempty"`
	Source    string            `json:"source"`
	SessionID string            `json:"session_id,omitempty"`
	Output    []json.RawMessage `json:"output,omitempty"`
}

func (r *OpenAIResponse) Clone() *OpenAIResponse {
	if r == nil {
		return nil
	}
	clone := *r
	if r.Output != nil {
		clone.Output = make([]json.RawMessage, len(r.Output))
		for i, item := range r.Output {
			clone.Output[i] = append(json.RawMessage(nil), item...)
		}
	}
	return &clone
}
