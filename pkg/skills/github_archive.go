package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/atomicfile"
)

const (
	githubArchiveLimit  = 32 << 20
	githubExpandedLimit = 128 << 20
	githubFileLimit     = 1 << 20
	githubFilesLimit    = 4096
)

func (s githubSource) download(ctx context.Context, commit, snapshot string) error {
	resp, err := githubGet(ctx, "https://codeload.github.com/"+s.repository+"/tar.gz/"+commit, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	archive, err := readGitHubBody(resp.Body, githubArchiveLimit)
	if err != nil {
		return err
	}
	roots, err := s.archiveRoots(ctx, archive)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(snapshot), 0o700); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(snapshot), ".download-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := extractGitHubSkills(ctx, archive, roots, filepath.Join(staging, "content")); err != nil {
		return err
	}
	metadata, err := json.Marshal(roots)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(filepath.Join(staging, "roots.json"), bytes.NewReader(metadata), 0o600); err != nil {
		return err
	}
	if _, err := s.readSnapshot(staging); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, "complete"), nil, 0o600); err != nil {
		return err
	}
	if err := os.Rename(staging, snapshot); err != nil {
		// Another process may have published this immutable snapshot first.
		if _, statErr := os.Stat(filepath.Join(snapshot, "complete")); statErr != nil {
			return fmt.Errorf("publishing GitHub snapshot: %w", err)
		}
	}
	return nil
}

func (s githubSource) archiveRoots(ctx context.Context, archive []byte) ([]string, error) {
	var candidates []string
	err := walkGitHubArchive(ctx, archive, func(header *tar.Header, name string, _ io.Reader) error {
		if header.Typeflag != tar.TypeReg || path.Base(name) != skillFile {
			return nil
		}
		if s.directory != "" && !withinGitHubPath(name, s.directory) {
			return nil
		}
		candidates = append(candidates, path.Dir(name))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.directory == "" {
		for _, prefix := range []string{"skills", ".agents/skills", "."} {
			var roots []string
			for _, candidate := range candidates {
				if prefix == "." && candidate == "." || prefix != "." && withinGitHubPath(candidate, prefix) {
					roots = append(roots, candidate)
				}
			}
			if len(roots) != 0 {
				candidates = roots
				break
			}
			if prefix == "." {
				candidates = nil
			}
		}
	}
	if len(candidates) == 0 {
		return nil, errors.New("no SKILL.md files found in the GitHub skill source")
	}
	slices.Sort(candidates)
	return candidates, nil
}

func withinGitHubPath(name, directory string) bool {
	return directory == "." || name == directory || strings.HasPrefix(name, directory+"/")
}

func walkGitHubArchive(ctx context.Context, archive []byte, visit func(*tar.Header, string, io.Reader) error) error {
	compressed, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("opening GitHub archive: %w", err)
	}
	defer compressed.Close()
	limited := &io.LimitedReader{R: compressed, N: githubExpandedLimit + 1}
	reader := tar.NewReader(limited)
	var prefix string
	for entries := 0; ; entries++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if limited.N <= 1 || entries > 100000 {
			return errors.New("GitHub archive exceeds expansion limit")
		}
		if errors.Is(err, io.EOF) {
			// tar EOF precedes the gzip trailer; drain to verify its checksum.
			if _, err := io.Copy(io.Discard, limited); err != nil {
				return fmt.Errorf("validating GitHub archive: %w", err)
			}
			if limited.N <= 1 {
				return errors.New("GitHub archive exceeds expansion limit")
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading GitHub archive: %w", err)
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		root, name, ok := strings.Cut(strings.TrimSuffix(header.Name, "/"), "/")
		if !ok {
			if header.Typeflag == tar.TypeDir && prefix == "" {
				prefix = root
				continue
			}
			return errors.New("invalid GitHub archive root")
		}
		if prefix == "" {
			prefix = root
		}
		if root != prefix || name != path.Clean(name) || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe GitHub archive path %q", header.Name)
		}
		if err := visit(header, name, reader); err != nil {
			return err
		}
	}
}

func extractGitHubSkills(ctx context.Context, archive []byte, roots []string, destination string) error {
	return extractGitHubSkillsWithFileLimit(ctx, archive, roots, destination, githubFilesLimit)
}

func extractGitHubSkillsWithFileLimit(ctx context.Context, archive []byte, roots []string, destination string, fileLimit int) error {
	seen := make(map[string]string)
	var files int
	var total int64
	return walkGitHubArchive(ctx, archive, func(header *tar.Header, name string, reader io.Reader) error {
		selected := slices.ContainsFunc(roots, func(root string) bool { return withinGitHubPath(name, root) })
		if !selected {
			return nil
		}
		if !validGitHubPath(name) {
			return fmt.Errorf("unsafe GitHub skill path %q", name)
		}
		if header.Typeflag != tar.TypeDir && header.Typeflag != tar.TypeReg {
			return fmt.Errorf("unsupported GitHub archive entry %q (links are not allowed)", name)
		}
		// Check every component, not just files, for case-insensitive collisions.
		for component := name; component != "."; component = path.Dir(component) {
			folded := strings.ToLower(component)
			if original, ok := seen[folded]; ok && original != component {
				return fmt.Errorf("colliding GitHub archive paths %q and %q", original, component)
			}
			seen[folded] = component
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		if header.Typeflag == tar.TypeDir {
			return os.MkdirAll(target, 0o700)
		}
		files++
		total += header.Size
		if header.Size < 0 || header.Size > githubFileLimit || total > githubArchiveLimit || files > fileLimit {
			return errors.New("GitHub skill files exceed download limits")
		}
		body, err := readGitHubBody(reader, githubFileLimit)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(body)
		closeErr := file.Close()
		return errors.Join(writeErr, closeErr)
	})
}

func (s githubSource) readSnapshot(snapshot string) ([]Skill, error) {
	content := filepath.Join(snapshot, "content")
	data, err := os.ReadFile(filepath.Join(snapshot, "roots.json"))
	if err != nil {
		return nil, err
	}
	var roots []string
	if err := json.Unmarshal(data, &roots); err != nil {
		return nil, err
	}
	var loaded []Skill
	for _, root := range roots {
		if root != "." && !validGitHubPath(root) {
			return nil, errors.New("invalid cached GitHub skill root")
		}
		filename := filepath.Join(content, filepath.FromSlash(root), skillFile)
		name := path.Base(root)
		if root == "." {
			name = path.Base(s.repository)
		}
		skill, ok := loadSkillFile(filename, name)
		if !ok {
			return nil, fmt.Errorf("invalid or missing GitHub skill %q", root)
		}
		loaded = append(loaded, skill)
	}
	if len(loaded) == 0 {
		return nil, errors.New("GitHub skill source contains no valid skills")
	}
	names := make(map[string]bool)
	for i := range loaded {
		skill := &loaded[i]
		if !isValidSkillName(skill.Name) || names[skill.Name] {
			return nil, fmt.Errorf("invalid or duplicate GitHub skill name %q", skill.Name)
		}
		names[skill.Name] = true
		skill.Local = false
		// Remote metadata must not activate extra toolsets or switch providers.
		skill.Model = ""
		skill.Toolsets = nil
		err := filepath.WalkDir(skill.BaseDir, func(filename string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(skill.BaseDir, filename)
			if err != nil {
				return err
			}
			skill.Files = append(skill.Files, filepath.ToSlash(relative))
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return loaded, nil
}
