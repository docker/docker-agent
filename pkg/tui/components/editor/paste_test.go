package editor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	"github.com/docker/go-units"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandlePaste_SmallContent(t *testing.T) {
	t.Parallel()

	e := &editor{}
	// Content that's under both limits: few lines and few chars
	smallContent := "line1\nline2\nline3"

	handled := e.handlePaste(smallContent)

	assert.False(t, handled, "small content should not be handled (return false)")
	assert.Empty(t, e.attachments, "no attachments should be created for small content")
}

func TestHandlePaste_AtLineLimitIsInline(t *testing.T) {
	t.Parallel()

	e := &editor{}
	// Exactly at line limit (10 lines) and under char limit should be inline
	lines := make([]string, maxInlinePasteLines)
	for i := range lines {
		lines[i] = "short"
	}
	content := strings.Join(lines, "\n")

	handled := e.handlePaste(content)

	assert.False(t, handled, "content at line limit should be inline")
}

func TestHandlePaste_AtCharLimitIsInline(t *testing.T) {
	t.Parallel()

	e := &editor{}
	// Exactly at char limit and under line limit should be inline
	content := strings.Repeat("x", maxInlinePasteChars)

	handled := e.handlePaste(content)

	assert.False(t, handled, "content at char limit should be inline")
}

func TestHandlePaste_ExceedsLineLimit(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	pastesDir := filepath.Join(tmpDir, "pastes")

	e := &editor{}
	// Content exceeding line limit (11 lines)
	lines := make([]string, maxInlinePasteLines+1)
	for i := range lines {
		lines[i] = "short"
	}
	largeContent := strings.Join(lines, "\n")

	// Create paste buffer directly to test without textarea
	att, err := createPasteAttachmentInDir(pastesDir, largeContent)
	require.NoError(t, err)

	e.attachments = append(e.attachments, att)

	// Verify file was created
	assert.FileExists(t, att.path)

	// Verify attachment struct is correct
	assert.NotEmpty(t, att.path)
	assert.True(t, strings.HasPrefix(att.placeholder, "@"))
	assert.True(t, att.isTemp, "paste attachments should be marked as temp")
}

func TestHandlePaste_ExceedsCharLimit(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	pastesDir := filepath.Join(tmpDir, "pastes")

	e := &editor{}
	// Single line exceeding char limit
	largeContent := strings.Repeat("x", maxInlinePasteChars+1)

	// Create paste buffer directly to test without textarea
	att, err := createPasteAttachmentInDir(pastesDir, largeContent)
	require.NoError(t, err)

	e.attachments = append(e.attachments, att)

	// Verify file was created
	assert.FileExists(t, att.path)

	// Verify attachment struct is correct
	assert.NotEmpty(t, att.path)
	assert.True(t, strings.HasPrefix(att.placeholder, "@"))
	assert.NotEmpty(t, att.label, "label should show size")
	assert.True(t, att.isTemp, "paste attachments should be marked as temp")
}

func TestCollectAttachments_WithPastes(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	pastesDir := filepath.Join(tmpDir, "pastes")

	content := "This is the pasted content"
	att, err := createPasteAttachmentInDir(pastesDir, content)
	require.NoError(t, err)

	e := &editor{
		attachments: []attachment{att},
	}

	input := "Hello " + att.placeholder + " world"
	result := e.collectAttachments(input)

	// Paste content should be in the attachment's inline Content field
	require.Len(t, result, 1)
	assert.Equal(t, content, result[0].Content)
	assert.Empty(t, result[0].FilePath, "paste attachments should not have a file path")
	assert.NoFileExists(t, att.path, "paste file should be removed after collection")
	assert.Empty(t, e.attachments, "attachments should be cleared")
}

func TestCollectAttachments_RemovesUnusedFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	pastesDir := filepath.Join(tmpDir, "pastes")

	att, err := createPasteAttachmentInDir(pastesDir, "unused content")
	require.NoError(t, err)

	e := &editor{
		attachments: []attachment{att},
	}

	// Collect with content that doesn't include the placeholder
	result := e.collectAttachments("no placeholder here")

	assert.Empty(t, result)
	assert.NoFileExists(t, att.path, "unused paste file should be removed")
	assert.Empty(t, e.attachments)
}

func TestCleanup_RemovesAllPasteFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	pastesDir := filepath.Join(tmpDir, "pastes")

	att1, err := createPasteAttachmentInDir(pastesDir, "content 1")
	require.NoError(t, err)

	att2, err := createPasteAttachmentInDir(pastesDir, "content 2")
	require.NoError(t, err)

	e := &editor{
		attachments: []attachment{att1, att2},
	}

	// Verify files exist before cleanup
	assert.FileExists(t, att1.path)
	assert.FileExists(t, att2.path)

	e.Cleanup()

	// Verify files are removed after cleanup
	assert.NoFileExists(t, att1.path, "att1 should be removed")
	assert.NoFileExists(t, att2.path, "att2 should be removed")
	assert.Empty(t, e.attachments, "attachments should be cleared")
}

func TestCleanup_HandlesEmptyPastes(t *testing.T) {
	t.Parallel()

	e := &editor{}

	// Should not panic
	e.Cleanup()

	assert.Empty(t, e.attachments)
}

// createPasteAttachmentInDir is a test helper that creates a paste attachment in a specific directory.
func createPasteAttachmentInDir(dir, content string) (attachment, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return attachment{}, err
	}

	file, err := os.CreateTemp(dir, "paste-*.txt")
	if err != nil {
		return attachment{}, err
	}
	defer file.Close()

	if _, err := file.WriteString(content); err != nil {
		return attachment{}, err
	}

	path := file.Name()
	return attachment{
		path:        path,
		placeholder: "@" + path,
		label:       filepath.Base(path) + " (" + units.HumanSize(float64(len(content))) + ")",
		isTemp:      true,
	}, nil
}

// File path parsing tests for drag-and-drop feature
func TestParsePastedFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: nil,
		},
		{
			name:     "single file path",
			input:    "/path/to/file.png",
			expected: []string{"/path/to/file.png"},
		},
		{
			name:     "multiple space-separated",
			input:    "/path/to/file1.png /path/to/file2.jpg",
			expected: []string{"/path/to/file1.png", "/path/to/file2.jpg"},
		},
		{
			name:     "escaped spaces (Unix)",
			input:    `/path/to/my\ file.png`,
			expected: []string{"/path/to/my file.png"},
		},
		{
			name:     "multiple with escaped spaces",
			input:    `/path/to/file\ 1.png /path/to/file\ 2.jpg`,
			expected: []string{"/path/to/file 1.png", "/path/to/file 2.jpg"},
		},
		{
			name:     "newline separated",
			input:    "/path/to/file1.png\n/path/to/file2.jpg",
			expected: []string{"/path/to/file1.png", "/path/to/file2.jpg"},
		},
		{
			name:     "trailing backslash",
			input:    "/path/to/file.png\\",
			expected: []string{"/path/to/file.png"},
		},
		{
			name:     "null chars removed",
			input:    "/path/to/file\x00.png",
			expected: []string{"/path/to/file.png"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := ParsePastedFiles(tt.input)
			if len(result) != len(tt.expected) {
				t.Errorf("ParsePastedFiles() got %d paths, expected %d", len(result), len(tt.expected))
				t.Errorf("  got: %v", result)
				t.Errorf("  expected: %v", tt.expected)
				return
			}
			for i, path := range result {
				if path != tt.expected[i] {
					t.Errorf("ParsePastedFiles() path[%d] = %q, expected %q", i, path, tt.expected[i])
				}
			}
		})
	}
}

func TestWindowsTerminalParsePastedFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "single quoted path",
			input:    `"C:\path\to\file.png"`,
			expected: []string{`C:\path\to\file.png`},
		},
		{
			name:     "multiple quoted paths",
			input:    `"C:\path\to\file1.png" "C:\path\to\file2.jpg"`,
			expected: []string{`C:\path\to\file1.png`, `C:\path\to\file2.jpg`},
		},
		{
			name:     "unclosed quotes",
			input:    `"C:\path\to\file.png`,
			expected: nil,
		},
		{
			name:     "text outside quotes",
			input:    `"C:\path\to\file.png" extra`,
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := windowsTerminalParsePastedFiles(tt.input)
			if len(result) != len(tt.expected) {
				t.Errorf("windowsTerminalParsePastedFiles() got %d paths, expected %d", len(result), len(tt.expected))
				t.Errorf("  got: %v", result)
				t.Errorf("  expected: %v", tt.expected)
				return
			}
			for i, path := range result {
				if path != tt.expected[i] {
					t.Errorf("windowsTerminalParsePastedFiles() path[%d] = %q, expected %q", i, path, tt.expected[i])
				}
			}
		})
	}
}

func TestIsSupportedFileType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path     string
		expected bool
	}{
		{"/path/to/image.png", true},
		{"/path/to/image.jpg", true},
		{"/path/to/image.JPEG", true}, // Case insensitive
		{"/path/to/doc.pdf", true},
		{"/path/to/file.txt", false}, // Not supported yet
		{"/path/to/script.sh", false},
		{"/path/to/noext", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			result := IsSupportedFileType(tt.path)
			if result != tt.expected {
				t.Errorf("IsSupportedFileType(%q) = %v, expected %v", tt.path, result, tt.expected)
			}
		})
	}
}

func TestParsePastedFilesWithRealFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	file1 := filepath.Join(tmpDir, "test1.png")
	file2 := filepath.Join(tmpDir, "test2.jpg")

	if err := os.WriteFile(file1, []byte("fake png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file2, []byte("fake jpg"), 0o644); err != nil {
		t.Fatal(err)
	}

	input := file1 + "\n" + file2
	result := ParsePastedFiles(input)

	if len(result) != 2 {
		t.Errorf("Expected 2 paths, got %d", len(result))
	}
	if len(result) >= 2 {
		if result[0] != file1 {
			t.Errorf("Expected first path to be %q, got %q", file1, result[0])
		}
		if result[1] != file2 {
			t.Errorf("Expected second path to be %q, got %q", file2, result[1])
		}
	}
}

func TestValidateFilePath(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	regularFile := filepath.Join(tmpDir, "regular.txt")
	if err := os.WriteFile(regularFile, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	symlink := filepath.Join(tmpDir, "symlink.txt")
	if err := os.Symlink(regularFile, symlink); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"regular file", regularFile, false},
		{"symlink rejected", symlink, true},
		{"path traversal rejected", filepath.Join(tmpDir, "..", "etc", "passwd"), true},
		{"nonexistent file", filepath.Join(tmpDir, "nonexistent.txt"), true},
		{"directory", tmpDir, false}, // validateFilePath itself doesn't reject dirs; callers do
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := validateFilePath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateFilePath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
		})
	}
}

func TestValidateFilePath_TraversalBeforeClean(t *testing.T) {
	t.Parallel()

	// This is the specific exploit the reviewer flagged:
	// filepath.Clean resolves ".." so checking after Clean is useless.
	// We must reject before Clean.
	_, err := validateFilePath("/tmp/app/../../../etc/passwd")
	if err == nil {
		t.Error("expected validateFilePath to reject path traversal, but it succeeded")
	}
}

func TestAddFileAttachment_SizeLimit(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Exactly 5MB — should be rejected (>= 5MB)
	exactly5MB := filepath.Join(tmpDir, "exact5mb.png")
	if err := os.WriteFile(exactly5MB, make([]byte, 5*1024*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	// Just under 5MB — should be accepted
	justUnder := filepath.Join(tmpDir, "under5mb.png")
	if err := os.WriteFile(justUnder, make([]byte, 5*1024*1024-1), 0o644); err != nil {
		t.Fatal(err)
	}

	e := &editor{}

	err := e.addFileAttachment("@" + exactly5MB)
	if err == nil {
		t.Error("expected addFileAttachment to reject file exactly 5MB, but it succeeded")
	}

	err = e.addFileAttachment("@" + justUnder)
	if err != nil {
		t.Errorf("expected addFileAttachment to accept file just under 5MB, got error: %v", err)
	}
}

func newPasteTestEditor() *editor {
	ta := textarea.New()
	ta.Focus()
	return &editor{
		textarea: ta,
		banner:   newAttachmentBanner(),
	}
}

func TestHandlePaste_DragDropSingleFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	file := filepath.Join(tmpDir, "photo.png")
	require.NoError(t, os.WriteFile(file, []byte("PNG"), 0o644))

	e := newPasteTestEditor()
	handled := e.handlePaste(file)

	assert.True(t, handled, "valid file path should be handled as drag-and-drop")
	assert.Len(t, e.attachments, 1)
	assert.Contains(t, e.textarea.Value(), "@"+file)
}

func TestHandlePaste_DragDropMultipleFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	file1 := filepath.Join(tmpDir, "a.png")
	file2 := filepath.Join(tmpDir, "b.jpg")
	require.NoError(t, os.WriteFile(file1, []byte("PNG"), 0o644))
	require.NoError(t, os.WriteFile(file2, []byte("JPG"), 0o644))

	e := newPasteTestEditor()
	handled := e.handlePaste(file1 + " " + file2)

	assert.True(t, handled)
	assert.Len(t, e.attachments, 2)
	assert.Contains(t, e.textarea.Value(), "@"+file1)
	assert.Contains(t, e.textarea.Value(), "@"+file2)
}

func TestHandlePaste_RollbackOnPartialFailure(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	goodFile := filepath.Join(tmpDir, "valid.png")
	require.NoError(t, os.WriteFile(goodFile, []byte("PNG"), 0o644))

	// Second file is too large (>= 5MB)
	bigFile := filepath.Join(tmpDir, "huge.png")
	require.NoError(t, os.WriteFile(bigFile, make([]byte, 5*1024*1024), 0o644))

	e := newPasteTestEditor()
	handled := e.handlePaste(goodFile + " " + bigFile)

	assert.False(t, handled, "should fall through to text paste when any file fails")
	assert.Empty(t, e.attachments, "partial attachments should be rolled back")
	assert.NotContains(t, e.textarea.Value(), "@"+goodFile,
		"rolled-back placeholder text should be removed from textarea")
}

func TestHandlePaste_UnsupportedTypeRollback(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	png := filepath.Join(tmpDir, "ok.png")
	sh := filepath.Join(tmpDir, "script.sh")
	require.NoError(t, os.WriteFile(png, []byte("PNG"), 0o644))
	require.NoError(t, os.WriteFile(sh, []byte("#!/bin/sh"), 0o644))

	e := newPasteTestEditor()
	handled := e.handlePaste(png + " " + sh)

	assert.False(t, handled, "unsupported file type should cause fallback to text")
	assert.Empty(t, e.attachments, "no attachments when file type is unsupported")
	assert.Empty(t, e.textarea.Value(), "textarea should be clean after rollback")
}

func TestHandlePaste_SymlinkRejected(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	realFile := filepath.Join(tmpDir, "real.png")
	link := filepath.Join(tmpDir, "link.png")
	require.NoError(t, os.WriteFile(realFile, []byte("PNG"), 0o644))
	require.NoError(t, os.Symlink(realFile, link))

	e := newPasteTestEditor()
	handled := e.handlePaste(link)

	assert.False(t, handled, "symlink should be rejected")
	assert.Empty(t, e.attachments)
}

func TestHandlePaste_PathTraversalRejected(t *testing.T) {
	t.Parallel()

	e := newPasteTestEditor()
	handled := e.handlePaste("../../etc/passwd")

	assert.False(t, handled, "path traversal should be rejected")
	assert.Empty(t, e.attachments)
}

func TestRemoveLastNAttachments_CleansTextarea(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	file1 := filepath.Join(tmpDir, "a.png")
	file2 := filepath.Join(tmpDir, "b.png")
	require.NoError(t, os.WriteFile(file1, []byte("PNG"), 0o644))
	require.NoError(t, os.WriteFile(file2, []byte("PNG"), 0o644))

	e := newPasteTestEditor()
	require.NoError(t, e.AttachFile(file1))
	require.NoError(t, e.AttachFile(file2))

	assert.Len(t, e.attachments, 2)
	assert.Contains(t, e.textarea.Value(), "@"+file1)
	assert.Contains(t, e.textarea.Value(), "@"+file2)

	// Roll back the last one
	e.removeLastNAttachments(1)

	assert.Len(t, e.attachments, 1)
	assert.Contains(t, e.textarea.Value(), "@"+file1, "first attachment should remain")
	assert.NotContains(t, e.textarea.Value(), "@"+file2, "second attachment should be removed")

	// Roll back the remaining one
	e.removeLastNAttachments(1)

	assert.Empty(t, e.attachments)
	assert.Empty(t, strings.TrimSpace(e.textarea.Value()), "textarea should be empty after full rollback")
}

func TestRemoveLastNAttachments_PreservesTempAttachments(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	file := filepath.Join(tmpDir, "file.png")
	require.NoError(t, os.WriteFile(file, []byte("PNG"), 0o644))

	e := newPasteTestEditor()

	// Simulate a temp (paste) attachment
	e.attachments = append(e.attachments, attachment{
		path:        "/tmp/paste-1.txt",
		placeholder: "@paste-1",
		isTemp:      true,
	})

	// Add a real file attachment
	require.NoError(t, e.AttachFile(file))
	assert.Len(t, e.attachments, 2)

	// Roll back 1 — should only remove the non-temp one
	e.removeLastNAttachments(1)

	assert.Len(t, e.attachments, 1)
	assert.True(t, e.attachments[0].isTemp, "temp attachment should be preserved")
}

func TestAttachFile_DuplicateRejected(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	file := filepath.Join(tmpDir, "dup.png")
	require.NoError(t, os.WriteFile(file, []byte("PNG"), 0o644))

	e := newPasteTestEditor()
	require.NoError(t, e.AttachFile(file))
	require.NoError(t, e.AttachFile(file)) // duplicate

	assert.Len(t, e.attachments, 1, "duplicate should not create second attachment")
}

func TestAttachFile_SetsCorrectLabel(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	data := make([]byte, 1024)
	file := filepath.Join(tmpDir, "labeled.png")
	require.NoError(t, os.WriteFile(file, data, 0o644))

	e := newPasteTestEditor()
	require.NoError(t, e.AttachFile(file))

	require.Len(t, e.attachments, 1)
	expectedLabel := fmt.Sprintf("labeled.png (%s)", units.HumanSize(float64(len(data))))
	assert.Equal(t, expectedLabel, e.attachments[0].label)
}
