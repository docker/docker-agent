package scheduler

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

type Schedule struct {
	ID       string        `json:"id"`
	Name     string        `json:"name,omitempty"`
	Prompt   string        `json:"prompt"`
	Spec     string        `json:"spec"`
	Interval time.Duration `json:"-"`
	NextFire time.Time     `json:"next_fire"`
}

func (s Schedule) Recurring() bool { return s.Interval > 0 }

type store struct {
	mu       sync.Mutex
	seq      int
	byID     map[string]*Schedule
	inFlight map[string]time.Time
	retryDue map[string]time.Time
}

func newStore() *store {
	return &store{
		byID:     make(map[string]*Schedule),
		inFlight: make(map[string]time.Time),
		retryDue: make(map[string]time.Time),
	}
}

func (s *store) add(name, prompt, when string, now time.Time) (Schedule, error) {
	next, interval, err := parseWhen(when, now)
	if err != nil {
		return Schedule{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	sc := &Schedule{
		ID:       fmt.Sprintf("sched-%d", s.seq),
		Name:     name,
		Prompt:   prompt,
		Spec:     strings.TrimSpace(when),
		Interval: interval,
		NextFire: next,
	}
	s.byID[sc.ID] = sc
	return *sc, nil
}

func (s *store) list() []Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Schedule, 0, len(s.byID))
	for _, sc := range s.byID {
		out = append(out, *sc)
	}
	sortSchedules(out)
	return out
}

func (s *store) cancel(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.byID[id]; !ok {
		return false
	}
	delete(s.byID, id)
	delete(s.inFlight, id)
	delete(s.retryDue, id)
	return true
}

func (s *store) claimDue(now time.Time) []Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()

	var due []Schedule
	for id, sc := range s.byID {
		if sc.NextFire.After(now) {
			continue
		}
		if _, ok := s.inFlight[id]; ok {
			continue
		}
		due = append(due, *sc)
		logicalDue := sc.NextFire
		if retryDue, ok := s.retryDue[id]; ok {
			logicalDue = retryDue
		}
		s.inFlight[id] = logicalDue
	}
	sortSchedules(due)
	return due
}

func (s *store) finishFire(id string, now time.Time, success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	due, ok := s.inFlight[id]
	if !ok {
		return
	}
	delete(s.inFlight, id)

	sc, ok := s.byID[id]
	if !ok {
		return
	}
	if !success {
		s.retryDue[id] = due
		sc.NextFire = now.Add(recallRetryDelay)
		return
	}
	delete(s.retryDue, id)
	if !sc.Recurring() {
		delete(s.byID, id)
		return
	}

	next := due.Add(sc.Interval)
	for !next.After(now) {
		next = next.Add(sc.Interval)
	}
	sc.NextFire = next
}

func (s *store) untilNext(now time.Time) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var soonest time.Time
	found := false
	for id, sc := range s.byID {
		if _, ok := s.inFlight[id]; ok {
			continue
		}
		if !found || sc.NextFire.Before(soonest) {
			soonest = sc.NextFire
			found = true
		}
	}
	if !found {
		return 0, false
	}
	if d := soonest.Sub(now); d > 0 {
		return d, true
	}
	return 0, true
}

func sortSchedules(list []Schedule) {
	slices.SortFunc(list, func(a, b Schedule) int {
		if a.NextFire.Equal(b.NextFire) {
			return strings.Compare(a.ID, b.ID)
		}
		if a.NextFire.Before(b.NextFire) {
			return -1
		}
		return 1
	})
}
