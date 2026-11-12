package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/docker/docker-agent/pkg/atomicfile"
	"github.com/docker/docker-agent/pkg/environment"
)

const githubRefTTL = 5 * time.Minute

var githubLoads singleflight.Group

type githubSource struct {
	repository string
	ref        string
	directory  string
}

type githubResolution struct {
	Commit    string    `json:"commit"`
	CheckedAt time.Time `json:"checked_at"`
}

// GitHub tree URLs use one escaped segment for the ref; query parameters also
// support branches containing slashes without confusing them with directories.
func parseGitHubSource(source string) (githubSource, bool, error) {
	u, err := url.Parse(source)
	if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
		return githubSource{}, false, nil
	}
	invalid := func() (githubSource, bool, error) {
		return githubSource{}, true, errors.New("expected https://github.com/owner/repo[/tree/ref/directory], optionally with ref and path query parameters")
	}
	if u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" {
		return invalid()
	}
	segments := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(segments) < 2 {
		return invalid()
	}
	for i, segment := range segments {
		segments[i], err = url.PathUnescape(segment)
		if err != nil {
			return invalid()
		}
	}
	segments[1] = strings.TrimSuffix(segments[1], ".git")
	for _, segment := range segments[:2] {
		if !isValidSkillName(segment) {
			return invalid()
		}
	}
	result := githubSource{repository: strings.ToLower(strings.Join(segments[:2], "/"))}
	if len(segments) > 2 {
		if len(segments) < 4 || segments[2] != "tree" {
			return invalid()
		}
		result.ref = segments[3]
		if len(segments) > 4 {
			result.directory = strings.Join(segments[4:], "/")
		}
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return invalid()
	}
	for key, values := range query {
		if len(values) != 1 || (key != "ref" && key != "path") {
			return invalid()
		}
	}
	if values, ok := query["ref"]; ok {
		if result.ref != "" || values[0] == "" {
			return invalid()
		}
		result.ref = values[0]
	}
	if values, ok := query["path"]; ok {
		if result.directory != "" || values[0] == "" {
			return invalid()
		}
		result.directory = values[0]
	}
	if strings.ContainsAny(result.ref, "\x00\r\n\t ") || (result.directory != "" && !validGitHubPath(result.directory)) {
		return invalid()
	}
	return result, true, nil
}

func isCommitSHA(ref string) bool {
	if len(ref) != 40 {
		return false
	}
	_, err := hex.DecodeString(ref)
	return err == nil
}

func (s githubSource) cacheKey() string {
	return "github:" + s.repository + "?ref=" + url.QueryEscape(s.ref) + "&path=" + url.QueryEscape(s.directory)
}

func loadGitHubSkills(ctx context.Context, source githubSource, cache *diskCache, env environment.Provider) ([]Skill, error) {
	key := cache.cacheDir(source.cacheKey(), "github")
	result := githubLoads.DoChan(key, func() (any, error) {
		loadCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		commit, err := source.resolve(loadCtx, cache, env)
		if err != nil {
			return nil, err
		}
		snapshot := cache.cacheDir("github:"+source.repository+"@"+commit, "snapshots")
		snapshot = filepath.Join(snapshot, githubDirectoryKey(source.directory))
		if _, err := os.Stat(filepath.Join(snapshot, "complete")); errors.Is(err, os.ErrNotExist) {
			if err := source.download(loadCtx, commit, snapshot); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, fmt.Errorf("reading GitHub snapshot: %w", err)
		}
		return source.readSnapshot(snapshot)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-result:
		if result.Err != nil {
			return nil, result.Err
		}
		return result.Val.([]Skill), nil
	}
}

func githubDirectoryKey(directory string) string {
	// Reuse the cache's URL hashing so repository paths never become cache paths.
	digest := sha256.Sum256([]byte(directory))
	return hex.EncodeToString(digest[:])
}

func (s githubSource) resolve(ctx context.Context, cache *diskCache, env environment.Provider) (string, error) {
	resolutionPath := filepath.Join(cache.cacheDir(s.cacheKey(), "github"), "resolution.json")
	var cached githubResolution
	if data, err := os.ReadFile(resolutionPath); err == nil {
		if json.Unmarshal(data, &cached) == nil && isCommitSHA(cached.Commit) &&
			(isCommitSHA(s.ref) && strings.EqualFold(s.ref, cached.Commit) || time.Since(cached.CheckedAt) < githubRefTTL) {
			return cached.Commit, nil
		}
	}
	var repository struct {
		Private       bool   `json:"private"`
		Visibility    string `json:"visibility"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := githubJSON(ctx, "https://api.github.com/repos/"+s.repository, env, &repository); err != nil {
		return "", err
	}
	if repository.Private || repository.Visibility != "public" {
		return "", errors.New("GitHub skills require a public repository")
	}
	ref := s.ref
	if ref == "" {
		ref = repository.DefaultBranch
	}
	if ref == "" {
		return "", errors.New("GitHub repository has no default branch")
	}
	commit := strings.ToLower(ref)
	if !isCommitSHA(ref) {
		var revision struct {
			SHA string `json:"sha"`
		}
		if err := githubJSON(ctx, "https://api.github.com/repos/"+s.repository+"/commits/"+url.PathEscape(ref), env, &revision); err != nil {
			return "", err
		}
		commit = revision.SHA
		if !isCommitSHA(commit) {
			return "", errors.New("GitHub returned an invalid commit SHA")
		}
	}
	data, err := json.Marshal(githubResolution{Commit: commit, CheckedAt: time.Now()})
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(resolutionPath), 0o700); err != nil {
		return "", err
	}
	if err := atomicfile.Write(resolutionPath, strings.NewReader(string(data)), 0o600); err != nil {
		return "", err
	}
	return commit, nil
}

func githubJSON(ctx context.Context, endpoint string, env environment.Provider, result any) error {
	resp, err := githubGet(ctx, endpoint, env)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := readGitHubBody(resp.Body, 1<<20)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("decoding GitHub response: %w", err)
	}
	return nil
}

func githubGet(ctx context.Context, endpoint string, env environment.Provider) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, err
	}
	if req.URL.Host == "api.github.com" {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if env != nil {
			if token, _ := env.Get(ctx, "GITHUB_TOKEN"); token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
		}
	}
	// Canonical GitHub endpoints need no redirects; never forward credentials.
	client := *skillsHTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("GitHub HTTP %d (check GITHUB_TOKEN and API rate limits)", resp.StatusCode)
		}
		return nil, fmt.Errorf("fetching %s: HTTP %d", endpoint, resp.StatusCode)
	}
	return resp, nil
}

func readGitHubBody(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("GitHub response exceeds size limit")
	}
	return body, nil
}

func validGitHubPath(value string) bool {
	if value == "" || value != path.Clean(value) || strings.HasPrefix(value, "/") {
		return false
	}
	for segment := range strings.SplitSeq(value, "/") {
		if segment == "." || segment == ".." || strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") {
			return false
		}
		for _, c := range segment {
			if c < 0x20 || strings.ContainsRune(`\:*?"<>|`, c) {
				return false
			}
		}
		base := strings.ToUpper(strings.SplitN(segment, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
			len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9' {
			return false
		}
	}
	return true
}
