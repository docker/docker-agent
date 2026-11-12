package board

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/docker/docker-agent/pkg/atomicfile"
	"github.com/docker/docker-agent/pkg/paths"
)

// ErrCardNotFound reports a lookup of a card that does not exist (anymore).
var ErrCardNotFound = errors.New("card not found")

// ErrCardBusy rejects a forward move of a busy card. It is checked under the
// store lock so a watcher flipping the status concurrently cannot slip a
// running card past the caller's check.
var ErrCardBusy = errors.New("cannot move a busy card forward")

// Store persists the board's cards as a JSON file under the data directory.
// All methods are safe for concurrent use and return copies, so callers and
// background watchers can never alias the same Card.
type Store struct {
	mu    sync.Mutex
	path  string
	cards []*Card
}

// StatePath returns the file the board persists its cards to.
func StatePath() string {
	return filepath.Join(paths.GetDataDir(), "board", "cards.json")
}

// OpenStore loads the card store from path, starting empty when the file
// does not exist yet.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read board state: %w", err)
	}
	if err := json.Unmarshal(data, &s.cards); err != nil {
		return nil, fmt.Errorf("parse board state %s: %w", path, err)
	}
	// Drop null entries (hand-edited or corrupted file) rather than panic;
	// they carry no data worth preserving.
	s.cards = slices.DeleteFunc(s.cards, func(c *Card) bool { return c == nil })
	for _, c := range s.cards {
		c.RepoPath = expandHome(c.RepoPath)
		c.Worktree = expandHome(c.Worktree)
	}
	return s, nil
}

// save persists the cards. Callers must hold s.mu. Paths under the current
// home are written ~-contracted so the state file stays valid across
// environments whose home differs (host vs. docker sandbox).
func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	cards := make([]*Card, len(s.cards))
	for i, c := range s.cards {
		clone := *c
		clone.RepoPath = contractHome(clone.RepoPath)
		clone.Worktree = contractHome(clone.Worktree)
		cards[i] = &clone
	}
	data, err := json.MarshalIndent(cards, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(s.path, bytes.NewReader(data), 0o644)
}

// indexOf returns the position of the card with the given id, or -1.
// Callers must hold s.mu.
func (s *Store) indexOf(id string) int {
	return slices.IndexFunc(s.cards, func(c *Card) bool { return c.ID == id })
}

// ListCards returns all cards in board order.
func (s *Store) ListCards() []*Card {
	s.mu.Lock()
	defer s.mu.Unlock()
	cards := make([]*Card, 0, len(s.cards))
	for _, c := range s.cards {
		clone := *c
		cards = append(cards, &clone)
	}
	return cards
}

// GetCard returns the card with the given id.
func (s *Store) GetCard(id string) (*Card, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.indexOf(id)
	if i < 0 {
		return nil, fmt.Errorf("%w: %s", ErrCardNotFound, id)
	}
	return new(*s.cards[i]), nil
}

// InsertCard appends a card to the board.
func (s *Store) InsertCard(c *Card) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := *c
	s.cards = append(s.cards, &clone)
	return s.save()
}

// RenameProject rewrites the project name on every card that references
// oldName, so a project rename keeps its cards attached. Saves only when a
// card actually changed.
func (s *Store) RenameProject(oldName, newName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, c := range s.cards {
		if c.Project == oldName {
			c.Project = newName
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.save()
}

// UpdateCardStatus persists only the status field of a card, so background
// watchers holding a stale snapshot of the card cannot revert concurrent
// edits. It reports whether the status actually changed.
func (s *Store) UpdateCardStatus(id string, status CardStatus) (bool, error) {
	return s.updateField(id, func(c *Card) bool {
		if c.Status == status {
			return false
		}
		c.Status = status
		return true
	})
}

// UpdateCardTitle persists only the title field of a card. Same rationale as
// [Store.UpdateCardStatus]. It reports whether the title actually changed.
func (s *Store) UpdateCardTitle(id, title string) (bool, error) {
	return s.updateField(id, func(c *Card) bool {
		if c.Title == title {
			return false
		}
		c.Title = title
		return true
	})
}

func (s *Store) updateField(id string, update func(*Card) bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.indexOf(id)
	if i < 0 {
		return false, fmt.Errorf("%w: %s", ErrCardNotFound, id)
	}
	if !update(s.cards[i]) {
		return false, nil
	}
	return true, s.save()
}

// DeleteCard removes a card. Deleting a missing card is a no-op.
func (s *Store) DeleteCard(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.indexOf(id)
	if i < 0 {
		return nil
	}
	s.cards = slices.Delete(s.cards, i, i+1)
	return s.save()
}

// MoveCard atomically moves a card to the given column and re-inserts it at
// the end of the board order. The card is re-read under the lock so the move
// preserves the current status; when requireIdle is set, a card whose
// watcher concurrently flipped it to busy is rejected with [ErrCardBusy].
// The updated card is returned.
func (s *Store) MoveCard(id, column string, requireIdle bool) (*Card, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.indexOf(id)
	if i < 0 {
		return nil, fmt.Errorf("%w: %s", ErrCardNotFound, id)
	}
	card := s.cards[i]
	if requireIdle && card.Status.Busy() {
		return nil, ErrCardBusy
	}
	card.Column = column
	s.cards = append(slices.Delete(s.cards, i, i+1), card)
	if err := s.save(); err != nil {
		return nil, err
	}
	return new(*card), nil
}
