package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"reflect"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/skills"
)

const (
	maxSkillResources  = 512
	maxSkillTotalBytes = 16 * 1024 * 1024
)

// validatedSkill is an immutable, spec-validated representation of a Skill entry.
// It owns URI resolution, manifest lookup, directory listing, and content verification.
type validatedSkill struct {
	entry    skillEntry
	root     string
	name     string
	desc     string
	manifest map[string]skillResource
}

func newValidatedSkill(entry skillEntry) (*validatedSkill, error) {
	root, finalSegment, ok := skillRootAndName(entry.URI)
	if !ok {
		return nil, fmt.Errorf("skill URI %q must have a scheme and end with /SKILL.md", entry.URI)
	}
	if entry.Frontmatter == nil {
		return nil, errors.New("missing frontmatter")
	}
	name, _ := entry.Frontmatter["name"].(string)
	desc, _ := entry.Frontmatter["description"].(string)
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("frontmatter missing non-empty name")
	}
	if strings.TrimSpace(desc) == "" {
		return nil, errors.New("frontmatter missing non-empty description")
	}
	if finalSegment != name {
		return nil, fmt.Errorf("skill URI final segment %q does not match frontmatter name %q", finalSegment, name)
	}
	if !entry.Resources.Present {
		return nil, errors.New("missing resources")
	}

	vs := &validatedSkill{
		entry: entry,
		root:  root,
		name:  name,
		desc:  desc,
	}
	if entry.Resources.Dynamic {
		return vs, nil
	}

	if len(entry.Resources.Manifest) == 0 {
		return nil, errors.New("resources manifest must not be empty")
	}
	if len(entry.Resources.Manifest) > maxSkillResources {
		return nil, fmt.Errorf("skill has %d resources, exceeding limit of %d", len(entry.Resources.Manifest), maxSkillResources)
	}

	manifest := make(map[string]skillResource, len(entry.Resources.Manifest))
	prefix := root + "/"
	var totalSize int64
	for _, r := range entry.Resources.Manifest {
		if r.URI == "" {
			return nil, errors.New("manifest resource missing uri")
		}
		if _, dup := manifest[r.URI]; dup {
			return nil, fmt.Errorf("duplicate manifest resource %q", r.URI)
		}
		rel, ok := strings.CutPrefix(r.URI, prefix)
		if !ok {
			return nil, fmt.Errorf("manifest resource %q escapes skill root %q", r.URI, root)
		}
		if !isSafeRelativeSkillPath(rel) {
			return nil, fmt.Errorf("manifest resource %q has invalid relative path", r.URI)
		}
		if r.Size < 0 {
			return nil, fmt.Errorf("manifest resource %q has negative size", r.URI)
		}
		if !isValidSHA256Digest(r.Digest) {
			return nil, fmt.Errorf("manifest resource %q has invalid digest %q", r.URI, r.Digest)
		}
		if r.Size > maxSkillTotalBytes-totalSize {
			return nil, fmt.Errorf("skill total size exceeds limit of %d bytes", maxSkillTotalBytes)
		}
		totalSize += r.Size
		manifest[r.URI] = r
	}
	if _, hasSkillMD := manifest[entry.URI]; !hasSkillMD {
		return nil, fmt.Errorf("resources manifest missing SKILL.md entry %q", entry.URI)
	}

	vs.manifest = manifest
	return vs, nil
}

func (s *validatedSkill) isDynamic() bool {
	return s.entry.Resources.Dynamic
}

func (s *validatedSkill) summary() skills.MCPSkillSummary {
	var relFiles []string
	if !s.isDynamic() {
		prefix := s.root + "/"
		for uri := range s.manifest {
			if uri == s.entry.URI {
				continue
			}
			relFiles = append(relFiles, strings.TrimPrefix(uri, prefix))
		}
		slices.Sort(relFiles)
	}
	return skills.MCPSkillSummary{
		URI:         s.entry.URI,
		Name:        s.name,
		Description: s.desc,
		Files:       relFiles,
		Dynamic:     s.isDynamic(),
	}
}

func (s *validatedSkill) resolveFileURI(rawPath string) (string, error) {
	p := strings.TrimSpace(rawPath)
	if p == "" {
		return "", errors.New("file path is required")
	}
	prefix := s.root + "/"
	if strings.Contains(p, "://") {
		rel, ok := strings.CutPrefix(p, prefix)
		if !ok {
			return "", fmt.Errorf("file URI %q is outside skill root %q", p, s.root)
		}
		if !isSafeRelativeSkillPath(rel) {
			return "", fmt.Errorf("invalid file URI %q", p)
		}
		return p, nil
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return "", fmt.Errorf("file path %q must be relative to the skill root", rawPath)
	}
	cleaned := path.Clean(p)
	if !isSafeRelativeSkillPath(cleaned) {
		return "", fmt.Errorf("file path %q escapes skill root", rawPath)
	}
	return prefix + cleaned, nil
}

func (s *validatedSkill) resolveDirURI(rawPath string) (string, error) {
	p := strings.TrimSpace(rawPath)
	if p == "" || p == "." || p == "./" {
		return s.root, nil
	}
	p = strings.TrimSuffix(p, "/")
	if strings.Contains(p, "://") {
		if p == s.root {
			return s.root, nil
		}
		prefix := s.root + "/"
		rel, ok := strings.CutPrefix(p, prefix)
		if !ok {
			return "", fmt.Errorf("directory URI %q is outside skill root %q", p, s.root)
		}
		if !isSafeRelativeSkillPath(rel) {
			return "", fmt.Errorf("invalid directory URI %q", p)
		}
		return p, nil
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return "", fmt.Errorf("directory path %q must be relative to the skill root", rawPath)
	}
	cleaned := path.Clean(p)
	if cleaned == "." {
		return s.root, nil
	}
	if !isSafeRelativeSkillPath(cleaned) {
		return "", fmt.Errorf("directory path %q escapes skill root", rawPath)
	}
	return s.root + "/" + cleaned, nil
}

func (s *validatedSkill) listDir(dirURI string) []*gomcp.Resource {
	prefix := dirURI + "/"
	seenDirs := make(map[string]bool)
	var entries []*gomcp.Resource
	for uri := range s.manifest {
		rest, ok := strings.CutPrefix(uri, prefix)
		if !ok {
			continue
		}
		if idx := strings.IndexByte(rest, '/'); idx >= 0 {
			dirName := rest[:idx]
			childDirURI := prefix + dirName
			if !seenDirs[childDirURI] {
				seenDirs[childDirURI] = true
				entries = append(entries, &gomcp.Resource{
					URI:      childDirURI,
					Name:     dirName,
					MIMEType: directoryMIMEType,
				})
			}
			continue
		}
		entries = append(entries, &gomcp.Resource{
			URI:  uri,
			Name: rest,
		})
	}
	slices.SortFunc(entries, func(a, b *gomcp.Resource) int {
		return strings.Compare(a.URI, b.URI)
	})
	return entries
}

func (s *validatedSkill) verifyFrontmatter(raw []byte) error {
	fmBytes, ok := extractYAMLFrontmatter(raw)
	if !ok {
		return errors.New("SKILL.md is missing YAML frontmatter")
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(fmBytes, &parsed); err != nil {
		return fmt.Errorf("parsing SKILL.md frontmatter: %w", err)
	}
	normParsed, err := normalizeJSONValue(parsed)
	if err != nil {
		return fmt.Errorf("normalizing SKILL.md frontmatter: %w", err)
	}
	normExpected, err := normalizeJSONValue(s.entry.Frontmatter)
	if err != nil {
		return fmt.Errorf("normalizing entry frontmatter: %w", err)
	}
	if !reflect.DeepEqual(normParsed, normExpected) {
		return errors.New("SKILL.md frontmatter does not match skill entry metadata")
	}
	return nil
}

func verifyDigestAndSize(raw []byte, expected skillResource) error {
	if int64(len(raw)) != expected.Size {
		return fmt.Errorf("size mismatch: got %d bytes, manifest expected %d", len(raw), expected.Size)
	}
	sum := sha256.Sum256(raw)
	actual := "sha256:" + hex.EncodeToString(sum[:])
	if actual != expected.Digest {
		return fmt.Errorf("digest mismatch: got %s, manifest expected %s", actual, expected.Digest)
	}
	return nil
}

func skillRootAndName(uri string) (root, finalSegment string, ok bool) {
	schemeSep := strings.Index(uri, "://")
	if schemeSep <= 0 || !strings.HasSuffix(uri, "/SKILL.md") {
		return "", "", false
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", false
	}
	root = strings.TrimSuffix(uri, "/SKILL.md")
	afterScheme := root[schemeSep+3:]
	// Support both authority-bearing URIs (skill://code-review/SKILL.md) and
	// empty-authority URIs (file:///skills/code-review/SKILL.md).
	trimmedPath := afterScheme
	if u.Host == "" {
		var hasLeadingSlash bool
		trimmedPath, hasLeadingSlash = strings.CutPrefix(afterScheme, "/")
		if !hasLeadingSlash {
			return "", "", false
		}
	}
	if trimmedPath == "" || strings.HasPrefix(trimmedPath, "/") || strings.HasSuffix(trimmedPath, "/") {
		return "", "", false
	}
	for seg := range strings.SplitSeq(trimmedPath, "/") {
		if !isSafeSkillPathSegment(seg) {
			return "", "", false
		}
	}
	if idx := strings.LastIndex(trimmedPath, "/"); idx >= 0 {
		finalSegment = trimmedPath[idx+1:]
	} else {
		finalSegment = trimmedPath
	}
	return root, finalSegment, finalSegment != ""
}

func isSafeRelativeSkillPath(rel string) bool {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.HasSuffix(rel, "/") || strings.Contains(rel, "\\") {
		return false
	}
	for seg := range strings.SplitSeq(rel, "/") {
		if !isSafeSkillPathSegment(seg) {
			return false
		}
	}
	return true
}

func isSafeSkillPathSegment(seg string) bool {
	if seg == "" || seg == "." || seg == ".." || strings.Contains(seg, "\\") {
		return false
	}
	decoded, err := url.PathUnescape(seg)
	if err != nil || decoded == "" || decoded == "." || decoded == ".." || strings.ContainsAny(decoded, `/\`) {
		return false
	}
	return true
}

func isValidSHA256Digest(digest string) bool {
	hexPart, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hexPart) != 64 {
		return false
	}
	for i := range len(hexPart) {
		ch := hexPart[i]
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func extractYAMLFrontmatter(raw []byte) ([]byte, bool) {
	normalized := strings.ReplaceAll(string(raw), "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	rest, ok := strings.CutPrefix(normalized, "---\n")
	if !ok {
		return nil, false
	}
	if block, _, found := strings.Cut(rest, "\n---\n"); found {
		return []byte(block), true
	}
	if block, ok := strings.CutSuffix(rest, "\n---"); ok {
		return []byte(block), true
	}
	return nil, false
}

func normalizeJSONValue(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
