package scheduler

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const minRecurringInterval = time.Minute

var presets = map[string]time.Duration{
	"minutely": time.Minute,
	"hourly":   time.Hour,
	"daily":    24 * time.Hour,
	"weekly":   7 * 24 * time.Hour,
}

func parseWhen(when string, now time.Time) (next time.Time, interval time.Duration, err error) {
	s := strings.TrimSpace(when)
	if s == "" {
		return time.Time{}, 0, errors.New("empty schedule spec")
	}

	if d, ok := presets[strings.ToLower(s)]; ok {
		return now.Add(d), d, nil
	}

	switch {
	case hasPrefixFold(s, "in:"):
		d, err := time.ParseDuration(strings.TrimSpace(s[len("in:"):]))
		if err != nil {
			return time.Time{}, 0, fmt.Errorf("invalid delay in %q: %w", when, err)
		}
		if d <= 0 {
			return time.Time{}, 0, fmt.Errorf("delay must be positive in %q", when)
		}
		return now.Add(d), 0, nil

	case hasPrefixFold(s, "every:"):
		d, err := time.ParseDuration(strings.TrimSpace(s[len("every:"):]))
		if err != nil {
			return time.Time{}, 0, fmt.Errorf("invalid interval in %q: %w", when, err)
		}
		if d <= 0 {
			return time.Time{}, 0, fmt.Errorf("interval must be positive in %q", when)
		}
		if d < minRecurringInterval {
			return time.Time{}, 0, fmt.Errorf("recurring interval must be at least %s in %q", minRecurringInterval, when)
		}
		return now.Add(d), d, nil

	case hasPrefixFold(s, "at:"):
		ts, err := time.Parse(time.RFC3339, strings.TrimSpace(s[len("at:"):]))
		if err != nil {
			return time.Time{}, 0, fmt.Errorf("invalid RFC3339 time in %q: %w", when, err)
		}
		if !ts.After(now) {
			return time.Time{}, 0, fmt.Errorf("scheduled time %s is not in the future", ts.Format(time.RFC3339))
		}
		return ts, 0, nil

	default:
		return time.Time{}, 0, fmt.Errorf(
			"unrecognized schedule %q: use in:<dur>, at:<RFC3339>, every:<dur>, or minutely/hourly/daily/weekly", when,
		)
	}
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}
