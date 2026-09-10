package gitbranch

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const watchDebounce = 100 * time.Millisecond

// Watcher reports changes to the current branch of a working directory.
type Watcher struct {
	mu      sync.RWMutex
	dir     string
	current string
	changes chan string
	setDir  chan setDirRequest
	done    <-chan struct{}
}

type setDirRequest struct {
	dir  string
	done chan string
}

// Watch starts watching Git metadata until ctx is canceled.
func Watch(ctx context.Context, dir string) (*Watcher, error) {
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	w := &Watcher{
		dir:     dir,
		current: Current(dir),
		changes: make(chan string, 1),
		setDir:  make(chan setDirRequest),
		done:    ctx.Done(),
	}
	w.watchHeadDir(fsWatcher)
	go w.run(ctx, fsWatcher)
	return w, nil
}

// Current returns the last observed branch.
func (w *Watcher) Current() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.current
}

// Changes returns branch values when the observed branch changes.
func (w *Watcher) Changes() <-chan string {
	return w.changes
}

// SetDir switches the watcher to another working directory.
func (w *Watcher) SetDir(dir string) string {
	done := make(chan string, 1)
	select {
	case w.setDir <- setDirRequest{dir: dir, done: done}:
		select {
		case current := <-done:
			return current
		case <-w.done:
			return Current(dir)
		}
	case <-w.done:
		return Current(dir)
	}
}

func (w *Watcher) run(ctx context.Context, fsWatcher *fsnotify.Watcher) {
	defer fsWatcher.Close()
	defer close(w.changes)

	var debounce <-chan time.Time
	var timer *time.Timer
	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case req := <-w.setDir:
			for _, path := range fsWatcher.WatchList() {
				_ = fsWatcher.Remove(path)
			}
			w.mu.Lock()
			w.dir = req.dir
			w.current = Current(req.dir)
			current := w.current
			w.mu.Unlock()
			w.watchHeadDir(fsWatcher)
			select {
			case <-w.changes:
			default:
			}
			req.done <- current
		case event, ok := <-fsWatcher.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) != "HEAD" {
				continue
			}
			if timer == nil {
				timer = time.NewTimer(watchDebounce)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(watchDebounce)
			}
			debounce = timer.C
		case <-debounce:
			debounce = nil
			w.refresh()
		case _, ok := <-fsWatcher.Errors:
			if !ok {
				return
			}
		}
	}
}

func (w *Watcher) watchHeadDir(fsWatcher *fsnotify.Watcher) {
	w.mu.RLock()
	dir := w.dir
	w.mu.RUnlock()
	if head := findHead(dir); head != "" {
		_ = fsWatcher.Add(filepath.Dir(head))
	}
}

func (w *Watcher) refresh() {
	w.mu.Lock()
	branch := Current(w.dir)
	if branch == w.current {
		w.mu.Unlock()
		return
	}
	w.current = branch
	w.mu.Unlock()

	select {
	case w.changes <- branch:
	default:
		<-w.changes
		w.changes <- branch
	}
}
