package hooks

import "github.com/docker/docker-agent/pkg/hooks/events"

// EventContract returns the public contract, adjusted for internal dispatch lanes.
func EventContract(event EventType) events.Contract {
	if event == EventPreToolUsePreYolo {
		c, _ := events.Lookup(string(EventPreToolUse))
		c.Rewrite = events.RewriteNone
		c.Metadata = true
		return c
	}
	c, _ := events.Lookup(string(event))
	return c
}
