package config

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// StableSourceKey reduces a Sources map key to its stable identity: the part
// that persists across variant selectors and caller/environment metadata.
//
// For URL references the identity is the URL's path (scheme + host + path); the
// entire query string and fragment are treated as volatile. This is what lets a
// session created under one variant (e.g. ?gordonTag=v9-light) resume under
// another (?gordonTag=v9-dev) after the server is relaunched with a different
// tag: only the query differs, so both share one identity. Treating the whole
// query as volatile — rather than an enumerated denylist — keeps this robust as
// new query parameters are introduced by callers.
//
// Keys for URL references are url.QueryEscape'd URLs; this decodes the key
// before parsing. For keys that are not URL references (local files, OCI refs,
// built-ins) or that cannot be decoded/parsed, the key is returned unchanged,
// so callers can compare identities without special-casing the source type.
//
// The resume fallback that consumes this (see the session manager's
// resolveSource) always prefers an exact key match and only uses the stable
// identity when it resolves unambiguously, so collapsing distinct query strings
// to one identity never silently selects the wrong side-by-side variant.
func StableSourceKey(key string) string {
	decoded, err := url.QueryUnescape(key)
	if err != nil {
		return key
	}
	if !IsURLReference(decoded) {
		return key
	}
	u, err := url.Parse(decoded)
	if err != nil {
		return key
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// IsOCIReference checks if the input is a valid OCI reference
func IsOCIReference(input string) bool {
	if isLocalFile(input) {
		return false
	}
	_, err := name.ParseReference(input)
	return err == nil
}

// IsDigestReference reports whether an OCI reference is pinned by digest
// (ref@sha256:...) rather than a tag.
func IsDigestReference(input string) bool {
	ref, err := name.ParseReference(input)
	if err != nil {
		return false
	}
	_, ok := ref.(name.Digest)
	return ok
}

// isLocalFile checks if the input is a local file
func isLocalFile(input string) bool {
	ext := strings.ToLower(filepath.Ext(input))
	// Check for known config file extensions or file descriptors
	if ext == ".yaml" || ext == ".yml" || ext == ".hcl" || strings.HasPrefix(input, "/dev/fd/") {
		return true
	}
	// Check if it exists as a file on disk
	s, err := os.Stat(input)
	return err == nil && !s.IsDir()
}

func fileNameWithoutExt(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// IsExternalReference reports whether the input is an external agent reference
// (OCI image or URL) rather than a local agent name defined in the same config.
// Local agent names never contain "/", so the slash check distinguishes them
// from OCI references like "myorg/agent:tag" or "docker.io/org/agent:v1".
// It also handles the "name:ref" syntax (e.g. "reviewer:myorg/review-pr").
func IsExternalReference(input string) bool {
	_, ref := ParseExternalAgentRef(input)
	return isExternalRef(ref)
}

// ParseExternalAgentRef parses an external agent reference that may include an
// explicit name prefix. The syntax is "name:reference" where name is a simple
// identifier (no slashes) and reference is an OCI reference or URL.
//
// If no explicit name is provided, the base name is derived from the reference:
//   - OCI refs: last path segment without tag (e.g. "myorg/review-pr" → "review-pr")
//   - URLs: filename without extension (e.g. "https://example.com/agent.yaml" → "agent")
//
// Examples:
//
//	ParseExternalAgentRef("reviewer:myorg/review-pr") → ("reviewer", "myorg/review-pr")
//	ParseExternalAgentRef("myorg/review-pr") → ("review-pr", "myorg/review-pr")
//	ParseExternalAgentRef("docker.io/myorg/myagent:v1") → ("myagent", "docker.io/myorg/myagent:v1")
//	ParseExternalAgentRef("https://example.com/agent.yaml") → ("agent", "https://example.com/agent.yaml")
func ParseExternalAgentRef(input string) (agentName, ref string) {
	// If the whole input is already a valid external reference, derive the name
	// from it without trying to split on ":".
	if isExternalRef(input) {
		return externalRefBaseName(input), input
	}

	// Check for explicit "name:reference" syntax.
	// A name prefix is identified by not containing "/" (distinguishing it from
	// OCI references or URLs which always contain slashes).
	if i := strings.Index(input, ":"); i > 0 {
		candidate := input[:i]
		if !strings.Contains(candidate, "/") {
			remainder := input[i+1:]
			if isExternalRef(remainder) {
				return candidate, remainder
			}
		}
	}

	// Fallback: return input as both name and ref (for local agent names).
	return input, input
}

// isExternalRef is the core check for whether a string is an external reference.
// It is used by both IsExternalReference and ParseExternalAgentRef to avoid
// circular dependencies.
func isExternalRef(input string) bool {
	return IsURLReference(input) || (strings.Contains(input, "/") && IsOCIReference(input))
}

// externalRefBaseName extracts a short agent name from an external reference.
//
//   - OCI: last path segment, tag/digest stripped
//     "myorg/review-pr" → "review-pr"
//     "docker.io/myorg/myagent:v1" → "myagent"
//
//   - URL: filename without extension
//     "https://example.com/agent.yaml" → "agent"
func externalRefBaseName(ref string) string {
	if IsURLReference(ref) {
		return fileNameWithoutExt(ref)
	}

	// OCI reference: strip tag or digest, then take last path segment.
	base := ref
	if i := strings.LastIndex(base, "@"); i >= 0 {
		base = base[:i]
	}
	if i := strings.LastIndex(base, ":"); i >= 0 {
		// Only strip if the colon is after the last slash (i.e. it's a tag, not a port).
		if j := strings.LastIndex(base, "/"); j < i {
			base = base[:i]
		}
	}
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return base
}
