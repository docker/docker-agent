package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	DefaultMaxSize    = 10 * 1024 * 1024 // 10MB
	DefaultMaxBackups = 3
)

// RotatingFile is an io.WriteCloser that rotates log files when they exceed a size limit.
type RotatingFile struct {
	path       string
	maxSize    int64
	maxBackups int

	mu   sync.Mutex
	file *os.File
	size int64
}

type Option func(*RotatingFile)

// NewRotatingFile creates a new rotating file writer.
func NewRotatingFile(path string, opts ...Option) (*RotatingFile, error) {
	r := &RotatingFile{
		path:       path,
		maxSize:    DefaultMaxSize,
		maxBackups: DefaultMaxBackups,
	}

	for _, opt := range opts {
		opt(r)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}

	if err := r.openFile(); err != nil {
		return nil, err
	}

	return r, nil
}

func (r *RotatingFile) openFile() error {
	file, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}

	r.file = file
	r.size = info.Size()
	return nil
}

func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.size+int64(len(p)) > r.maxSize {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}

	n, err := r.file.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file != nil {
		return r.file.Close()
	}
	return nil
}

func (r *RotatingFile) rotate() error {
	if err := r.file.Close(); err != nil {
		return err
	}

	// Remove the oldest backup if it exists
	oldest := fmt.Sprintf("%s.%d", r.path, r.maxBackups)
	_ = os.Remove(oldest)

	// Shift existing backups: .2 -> .3, .1 -> .2, etc.
	for i := r.maxBackups - 1; i >= 1; i-- {
		oldPath := fmt.Sprintf("%s.%d", r.path, i)
		newPath := fmt.Sprintf("%s.%d", r.path, i+1)
		_ = os.Rename(oldPath, newPath)
	}

	// Rename current log to .1
	if err := os.Rename(r.path, r.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}

	r.size = 0
	return r.openFile()
}
