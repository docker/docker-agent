// Package httpsec provides HTTP security primitives for serving commands.
package httpsec

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// BearerAuth authenticates requests with a static bearer token.
func BearerAuth(token string) func(http.Handler) http.Handler {
	expected := []byte(token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(got), expected) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// OriginMatcher holds validated literal origins and compiled origin patterns.
type OriginMatcher struct {
	literals []string
	patterns []*regexp.Regexp
}

// ParseOrigins parses a comma-separated list of literal origins and regular
// expression patterns prefixed with "~".
func ParseOrigins(spec string) (*OriginMatcher, error) {
	matcher := &OriginMatcher{}
	for raw := range strings.SplitSeq(spec, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if pattern, ok := strings.CutPrefix(entry, "~"); ok {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("invalid CORS regex %q: %w", pattern, err)
			}
			matcher.patterns = append(matcher.patterns, re)
			continue
		}
		if err := validateOrigin(entry); err != nil {
			return nil, err
		}
		matcher.literals = append(matcher.literals, entry)
	}
	if len(matcher.literals) == 0 && len(matcher.patterns) == 0 {
		return nil, errors.New("no usable CORS origins")
	}
	return matcher, nil
}

// Literals returns the configured literal origins.
func (m *OriginMatcher) Literals() []string {
	return slices.Clone(m.literals)
}

// HasPatterns reports whether the matcher has regular expression patterns.
func (m *OriginMatcher) HasPatterns() bool {
	return len(m.patterns) > 0
}

// MatchPattern reports whether origin matches a configured regular expression.
func (m *OriginMatcher) MatchPattern(origin string) bool {
	for _, pattern := range m.patterns {
		if pattern.MatchString(origin) {
			return true
		}
	}
	return false
}

func validateOrigin(origin string) error {
	if origin == "*" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("invalid CORS origin %q: %w", origin, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid CORS origin %q: scheme must be http or https", origin)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid CORS origin %q: missing host", origin)
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid CORS origin %q: must not include path, query, or fragment", origin)
	}
	return nil
}
