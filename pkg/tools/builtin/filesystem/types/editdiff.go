package types

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aymanbagabas/go-udiff"
)

// MaxEditFileMetaBytes leaves room for the event envelope and textual output in SSE frames.
const MaxEditFileMetaBytes = 256 * 1024

// EditFileMeta records the file change at execution time, independently of later edits.
type EditFileMeta struct {
	Path      string        `json:"path"`
	Hunks     []*udiff.Hunk `json:"hunks"`
	Added     int           `json:"added"`
	Removed   int           `json:"removed"`
	Truncated bool          `json:"truncated,omitempty"`
}

// ApplyEdits applies edits in order, just as edit_file does when writing a file.
func ApplyEdits(content string, edits []Edit) (string, error) {
	for i, edit := range edits {
		if edit.OldText == "" {
			return "", fmt.Errorf("Edit %d failed: oldText must not be empty", i+1)
		}
		if !strings.Contains(content, edit.OldText) {
			return "", fmt.Errorf("Edit %d failed: old text not found", i+1)
		}
		content = strings.Replace(content, edit.OldText, edit.NewText, 1)
	}
	return content, nil
}

func NewEditFileMeta(path, before, after string) (EditFileMeta, error) {
	edits := udiff.Lines(before, after)
	diff, err := udiff.ToUnifiedDiff(path, path, before, edits, 3)
	if err != nil {
		return EditFileMeta{}, fmt.Errorf("computing edit diff: %w", err)
	}
	meta := EditFileMeta{Path: path, Hunks: diff.Hunks}
	for _, hunk := range diff.Hunks {
		for _, line := range hunk.Lines {
			switch line.Kind {
			case udiff.Insert:
				meta.Added++
			case udiff.Delete:
				meta.Removed++
			}
		}
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return EditFileMeta{}, fmt.Errorf("encoding edit diff: %w", err)
	}
	if len(encoded) <= MaxEditFileMetaBytes {
		return meta, nil
	}

	// Remove context first so a huge unchanged line cannot crowd out a small edit.
	meta.Truncated = true
	diff, err = udiff.ToUnifiedDiff(path, path, before, edits, 0)
	if err != nil {
		return EditFileMeta{}, fmt.Errorf("computing compact edit diff: %w", err)
	}
	meta.Hunks = nil
	encoded, err = json.Marshal(meta)
	if err != nil || len(encoded) > MaxEditFileMetaBytes {
		return EditFileMeta{}, errors.New("edit diff header exceeds metadata limit")
	}
	budget := MaxEditFileMetaBytes - len(encoded) - 2
	for _, hunk := range diff.Hunks {
		encoded, err := json.Marshal(hunk)
		if err != nil {
			return EditFileMeta{}, fmt.Errorf("encoding edit hunk: %w", err)
		}
		if len(encoded)+1 > budget {
			continue
		}
		meta.Hunks = append(meta.Hunks, hunk)
		budget -= len(encoded) + 1
	}
	return meta, nil
}

// DecodeEditFileMeta accepts both in-process metadata and JSON-decoded session/API results.
func DecodeEditFileMeta(value any) (EditFileMeta, bool) {
	if meta, ok := value.(EditFileMeta); ok {
		return meta, true
	}
	if value == nil {
		return EditFileMeta{}, false
	}
	data, err := json.Marshal(value)
	if err != nil {
		return EditFileMeta{}, false
	}
	var meta EditFileMeta
	if json.Unmarshal(data, &meta) != nil || meta.Path == "" {
		return EditFileMeta{}, false
	}
	return meta, true
}
