package streamcontent

// Streams counts active streams per session, so a session's replies complete
// only when its last stream stops, even when nested streams share the session.
// The zero value is ready to use.
type Streams struct {
	active map[string]int
}

func (s *Streams) Start(sessionID string) {
	if s.active == nil {
		s.active = make(map[string]int)
	}
	s.active[sessionID]++
}

// Stop reports whether sessionID has no active stream left.
func (s *Streams) Stop(sessionID string) bool {
	if s.active[sessionID] > 1 {
		s.active[sessionID]--
		return false
	}
	delete(s.active, sessionID)
	return true
}

func (s *Streams) Reset() { clear(s.active) }
