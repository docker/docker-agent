package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/paths"
)

const githubTestCommit = "0123456789abcdef0123456789abcdef01234567"

// Preserve canonical URLs and headers while routing requests to loopback.
type githubTestTransport struct {
	base      http.RoundTripper
	serverURL *url.URL
}

func (transport githubTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || (req.URL.Host != "api.github.com" && req.URL.Host != "codeload.github.com") {
		return nil, fmt.Errorf("unexpected GitHub test endpoint %s", req.URL)
	}
	local := req.Clone(req.Context())
	local.URL.Scheme = transport.serverURL.Scheme
	local.URL.Host = transport.serverURL.Host
	local.Host = req.URL.Host
	return transport.base.RoundTrip(local)
}

func githubTestClient(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	previous := skillsHTTPClient
	client := *server.Client()
	client.Transport = githubTestTransport{base: client.Transport, serverURL: serverURL}
	skillsHTTPClient = &client
	t.Cleanup(func() { skillsHTTPClient = previous })
}

type githubTestEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
	size     int64
}

type githubZeroReader struct{}

func (githubZeroReader) Read(data []byte) (int, error) {
	clear(data)
	return len(data), nil
}

func githubTestArchive(t *testing.T, entries ...githubTestEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressed)
	for _, entry := range entries {
		kind := entry.typeflag
		if kind == 0 {
			kind = tar.TypeReg
		}
		size := max(int64(len(entry.body)), entry.size)
		if kind != tar.TypeReg {
			size = 0
		}
		header := &tar.Header{Name: entry.name, Mode: 0o777, Typeflag: kind, Linkname: entry.linkname, Size: size}
		if kind == tar.TypeXGlobalHeader {
			header = &tar.Header{Name: entry.name, Typeflag: kind, PAXRecords: map[string]string{"comment": "GitHub snapshot"}}
		}
		require.NoError(t, writer.WriteHeader(header))
		if kind == tar.TypeReg {
			_, err := io.WriteString(writer, entry.body)
			require.NoError(t, err)
			_, err = io.CopyN(writer, githubZeroReader{}, size-int64(len(entry.body)))
			require.NoError(t, err)
		}
	}
	require.NoError(t, writer.Close())
	require.NoError(t, compressed.Close())
	return buffer.Bytes()
}

func githubTestSkill(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n\n!`echo untrusted`\n"
}

func githubTestSnapshot(cache *diskCache, source githubSource, commit string) string {
	return filepath.Join(cache.cacheDir("github:"+source.repository+"@"+commit, "snapshots"), githubDirectoryKey(source.directory))
}

func githubTestExpireResolution(t *testing.T, cache *diskCache, source githubSource) {
	t.Helper()
	filename := filepath.Join(cache.cacheDir(source.cacheKey(), "github"), "resolution.json")
	data, err := os.ReadFile(filename)
	require.NoError(t, err)
	var resolution githubResolution
	require.NoError(t, json.Unmarshal(data, &resolution))
	resolution.CheckedAt = time.Now().Add(-2 * githubRefTTL)
	data, err = json.Marshal(resolution)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filename, data, 0o600))
}

func TestGitHubSourceParsing(t *testing.T) {
	valid := []struct {
		url       string
		ref       string
		directory string
	}{
		{url: "https://github.com/Owner/Repo"},
		{url: "https://github.com/Owner/Repo.git/"},
		{url: "https://GITHUB.COM/Owner/Repo/tree/main/skills", ref: "main", directory: "skills"},
		{url: "https://github.com/Owner/Repo/tree/feature%2Ffoo/skills/nested", ref: "feature/foo", directory: "skills/nested"},
		{url: "https://github.com/Owner/Repo?ref=feature%2Ffoo&path=skills", ref: "feature/foo", directory: "skills"},
		{url: "https://github.com/Owner/Repo/tree/v1.0.0", ref: "v1.0.0"},
		{url: "https://github.com/Owner/Repo?ref=" + githubTestCommit, ref: githubTestCommit},
	}
	for _, test := range valid {
		t.Run(test.url, func(t *testing.T) {
			source, recognized, err := parseGitHubSource(test.url)
			require.NoError(t, err)
			require.True(t, recognized)
			assert.Equal(t, githubSource{repository: "owner/repo", ref: test.ref, directory: test.directory}, source)
		})
	}
	invalid := []string{
		"http://github.com/owner/repo", "https://user:secret@github.com/owner/repo",
		"https://github.com:443/owner/repo", "https://github.com/owner/repo#fragment",
		"https://github.com/owner", "https://github.com/owner/repo/blob/main/SKILL.md",
		"https://github.com/owner/repo/tree", "https://github.com/owner/repo/tree/main?ref=other",
		"https://github.com/owner/repo?ref=", "https://github.com/owner/repo?path=",
		"https://github.com/owner/repo?ref=a&ref=b", "https://github.com/owner/repo?token=secret",
		"https://github.com/owner/repo?path=..%2Foutside", "https://github.com/owner/repo?path=%2Fabsolute",
		"https://github.com/owner/repo?path=skills%2F..%2Foutside", "https://github.com/owner/repo?path=skills%5Coutside",
		"https://github.com/owner/repo?ref=main%0Ainjected", "https://github.com/owner/repo?ref=with+space",
		"https://github.com/owner/repo?path=skills%2FCON.txt", "https://github.com/owner/repo?path=skills%2Ftrailing.",
	}
	for _, source := range invalid {
		t.Run(source, func(t *testing.T) {
			_, recognized, err := parseGitHubSource(source)
			assert.True(t, recognized)
			require.Error(t, err)
		})
	}
	for _, source := range []string{"https://example.com/skills", "https://github.com.evil.test/owner/repo", "https://api.github.com/repos/owner/repo"} {
		_, recognized, err := parseGitHubSource(source)
		require.NoError(t, err)
		assert.False(t, recognized)
	}
}

func TestGitHubLoadRefsAndWarmCache(t *testing.T) {
	for _, ref := range []string{"", "main", "v1.2.3", "feature/foo", strings.ToUpper(githubTestCommit)} {
		t.Run("ref="+ref, func(t *testing.T) {
			archive := githubTestArchive(t, githubTestEntry{name: "repo-commit/skills/build/SKILL.md", body: githubTestSkill("build", "Build images")})
			var requests []string
			githubTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Host+r.URL.EscapedPath())
				switch r.Host {
				case "api.github.com":
					assert.Equal(t, "Bearer provider-token", r.Header.Get("Authorization"))
					assert.Equal(t, "application/vnd.github+json", r.Header.Get("Accept"))
					assert.Equal(t, "2022-11-28", r.Header.Get("X-GitHub-Api-Version"))
					if r.URL.Path == "/repos/owner/repo" {
						fmt.Fprint(w, `{"private":false,"visibility":"public","default_branch":"trunk"}`)
					} else {
						fmt.Fprintf(w, `{"sha":%q}`, githubTestCommit)
					}
				case "codeload.github.com":
					assert.Empty(t, r.Header.Get("Authorization"))
					assert.Equal(t, "/owner/repo/tar.gz/"+githubTestCommit, r.URL.Path)
					_, _ = w.Write(archive)
				default:
					http.NotFound(w, r)
				}
			})
			t.Setenv("GITHUB_TOKEN", "must-not-use-process-token")
			env := environment.NewMapEnvProvider(map[string]string{"GITHUB_TOKEN": "provider-token"})
			cache := newDiskCache(t.TempDir())
			source := githubSource{repository: "owner/repo", ref: ref}
			loaded, err := loadGitHubSkills(t.Context(), source, cache, env)
			require.NoError(t, err)
			require.Len(t, loaded, 1)
			assert.Equal(t, "build", loaded[0].Name)
			wantRequests := []string{"api.github.com/repos/owner/repo"}
			if !isCommitSHA(ref) {
				resolvedRef := ref
				if resolvedRef == "" {
					resolvedRef = "trunk"
				}
				wantRequests = append(wantRequests, "api.github.com/repos/owner/repo/commits/"+url.PathEscape(resolvedRef))
			}
			wantRequests = append(wantRequests, "codeload.github.com/owner/repo/tar.gz/"+githubTestCommit)
			assert.Equal(t, wantRequests, requests)
			requests = nil
			if isCommitSHA(ref) {
				githubTestExpireResolution(t, cache, source)
			}
			warm, err := loadGitHubSkills(t.Context(), source, newDiskCache(cache.baseDir), environment.NewNoEnvProvider())
			require.NoError(t, err)
			assert.Equal(t, loaded, warm)
			assert.Empty(t, requests, "warm disk cache must perform no HTTP requests")
		})
	}
}

func TestGitHubRejectsNonPublicRepositories(t *testing.T) {
	for _, repository := range []string{
		`{"private":true,"visibility":"private","default_branch":"main"}`,
		`{"private":false,"visibility":"internal","default_branch":"main"}`,
		`{"private":true,"visibility":"public","default_branch":"main"}`,
		`{"private":false,"default_branch":"main"}`,
	} {
		t.Run(repository, func(t *testing.T) {
			requests := 0
			githubTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				assert.Equal(t, "api.github.com", r.Host)
				assert.Equal(t, "/repos/owner/repo", r.URL.Path)
				assert.Equal(t, "Bearer private-access-token", r.Header.Get("Authorization"))
				fmt.Fprint(w, repository)
			})
			cache := newDiskCache(t.TempDir())
			env := environment.NewMapEnvProvider(map[string]string{"GITHUB_TOKEN": "private-access-token"})
			loaded, err := loadGitHubSkills(t.Context(), githubSource{repository: "owner/repo", ref: githubTestCommit}, cache, env)
			require.ErrorContains(t, err, "public repository")
			assert.Empty(t, loaded)
			assert.Equal(t, 1, requests)
			entries, err := os.ReadDir(cache.baseDir)
			require.NoError(t, err)
			assert.Empty(t, entries, "rejected repositories must not create a cache")
		})
	}
}

func TestGitHubMutableRefRefresh(t *testing.T) {
	firstCommit := githubTestCommit
	secondCommit := strings.Repeat("b", 40)
	commit := firstCommit
	firstArchive := githubTestArchive(t, githubTestEntry{name: "repo-first/skills/build/SKILL.md", body: githubTestSkill("build", "First revision")})
	secondArchive := githubTestArchive(t, githubTestEntry{name: "repo-second/skills/build/SKILL.md", body: githubTestSkill("build", "Second revision")})
	var requests []string
	githubTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Host+r.URL.Path)
		switch r.URL.Path {
		case "/repos/owner/repo":
			fmt.Fprint(w, `{"private":false,"visibility":"public","default_branch":"main"}`)
		case "/repos/owner/repo/commits/main":
			fmt.Fprintf(w, `{"sha":%q}`, commit)
		case "/owner/repo/tar.gz/" + firstCommit:
			_, _ = w.Write(firstArchive)
		case "/owner/repo/tar.gz/" + secondCommit:
			_, _ = w.Write(secondArchive)
		default:
			http.NotFound(w, r)
		}
	})
	cache := newDiskCache(t.TempDir())
	source := githubSource{repository: "owner/repo", ref: "main"}
	first, err := loadGitHubSkills(t.Context(), source, cache, nil)
	require.NoError(t, err)
	require.Len(t, first, 1)
	commit = secondCommit
	githubTestExpireResolution(t, cache, source)
	requests = nil
	second, err := loadGitHubSkills(t.Context(), source, cache, nil)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, "Second revision", second[0].Description)
	assert.NotEqual(t, first[0].FilePath, second[0].FilePath)
	oldContent, err := os.ReadFile(first[0].FilePath)
	require.NoError(t, err)
	assert.Contains(t, string(oldContent), "First revision")
	require.Len(t, requests, 3)
	githubTestExpireResolution(t, cache, source)
	requests = nil
	_, err = loadGitHubSkills(t.Context(), source, cache, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"api.github.com/repos/owner/repo", "api.github.com/repos/owner/repo/commits/main"}, requests, "unchanged commit reuses its immutable snapshot")
}

func TestGitHubArchiveRootSelection(t *testing.T) {
	entries := []githubTestEntry{
		{name: "repo-commit/SKILL.md", body: githubTestSkill("root", "Root skill")},
		{name: "repo-commit/.agents/skills/agent/SKILL.md", body: githubTestSkill("agent", "Agent skill")},
		{name: "repo-commit/skills/zeta/SKILL.md", body: githubTestSkill("zeta", "Zeta skill")},
		{name: "repo-commit/skills/alpha/SKILL.md", body: githubTestSkill("alpha", "Alpha skill")},
		{name: "repo-commit/custom/selected/SKILL.md", body: githubTestSkill("selected", "Selected skill")},
		{name: "repo-commit/custom/selected-neighbor/SKILL.md", body: githubTestSkill("neighbor", "Must not select")},
	}
	for _, test := range []struct {
		name      string
		entries   []githubTestEntry
		directory string
		roots     []string
	}{
		{name: "skills wins", entries: entries, roots: []string{"skills/alpha", "skills/zeta"}},
		{name: "agents fallback", entries: entries[:2], roots: []string{".agents/skills/agent"}},
		{name: "root fallback", entries: entries[:1], roots: []string{"."}},
		{name: "selected skill", entries: entries, directory: "custom/selected", roots: []string{"custom/selected"}},
		{name: "selected collection", entries: entries, directory: "custom", roots: []string{"custom/selected", "custom/selected-neighbor"}},
		{name: "missing selection", entries: entries, directory: "missing"},
		{name: "no arbitrary fallback", entries: entries[4:]},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots, err := (githubSource{directory: test.directory}).archiveRoots(t.Context(), githubTestArchive(t, test.entries...))
			if test.roots == nil {
				require.ErrorContains(t, err, "no SKILL.md")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.roots, roots)
		})
	}
}

func TestGitHubSnapshotFieldsAndFiles(t *testing.T) {
	content := "---\ndescription: Build safely\nlicense: Apache-2.0\ncompatibility: Requires Docker\nmetadata:\n  author: upstream\nallowed-tools: Read, Grep\ncontext: fork\nmodel: openai/untrusted\ntoolsets: shell, secrets\n---\n\n!`echo untrusted`\n"
	archive := githubTestArchive(t,
		githubTestEntry{name: "repo-commit/skills/build/SKILL.md", body: content},
		githubTestEntry{name: "repo-commit/skills/build/references/guide.md", body: "Reference contents"},
		githubTestEntry{name: "repo-commit/skills/build/scripts/build.sh", body: "#!/bin/sh\necho build\n"},
		githubTestEntry{name: "repo-commit/README.md", body: "not part of the skill"},
		githubTestEntry{name: "repo-commit/unrelated/link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	)
	githubTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "codeload.github.com", r.Host)
		_, _ = w.Write(archive)
	})
	source := githubSource{repository: "owner/repo"}
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	require.NoError(t, source.download(t.Context(), githubTestCommit, snapshot))
	loaded, err := source.readSnapshot(snapshot)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	skill := loaded[0]
	assert.Equal(t, "build", skill.Name)
	assert.Equal(t, "Build safely", skill.Description)
	assert.False(t, skill.Local)
	assert.False(t, skill.ExpandsCommands())
	assert.False(t, skill.IsInline())
	assert.True(t, skill.IsFork())
	assert.Empty(t, skill.Model)
	assert.Nil(t, skill.Toolsets)
	assert.Equal(t, "Apache-2.0", skill.License)
	assert.Equal(t, "Requires Docker", skill.Compatibility)
	assert.Equal(t, map[string]string{"author": "upstream"}, skill.Metadata)
	assert.Equal(t, []string{"Read", "Grep"}, skill.AllowedTools)
	assert.Equal(t, []string{"SKILL.md", "references/guide.md", "scripts/build.sh"}, skill.Files)
	assert.Equal(t, filepath.Join(snapshot, "content", "skills", "build"), skill.BaseDir)
	assert.Equal(t, filepath.Join(skill.BaseDir, skillFile), skill.FilePath)
	body, err := os.ReadFile(skill.FilePath)
	require.NoError(t, err)
	assert.Equal(t, content, string(body))
	reference, err := os.ReadFile(filepath.Join(skill.BaseDir, "references", "guide.md"))
	require.NoError(t, err)
	assert.Equal(t, "Reference contents", string(reference))
	_, err = os.Stat(filepath.Join(snapshot, "complete"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(snapshot, "content", "README.md"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestGitHubRejectsUnsafeArchives(t *testing.T) {
	base := githubTestEntry{name: "repo-commit/skills/build/SKILL.md", body: githubTestSkill("build", "Safe skill")}
	for _, test := range []struct {
		name  string
		entry githubTestEntry
	}{
		{name: "parent traversal", entry: githubTestEntry{name: "repo-commit/../escaped", body: "bad"}},
		{name: "nested traversal", entry: githubTestEntry{name: "repo-commit/skills/build/../../escaped", body: "bad"}},
		{name: "absolute path", entry: githubTestEntry{name: "/absolute/escaped", body: "bad"}},
		{name: "backslash", entry: githubTestEntry{name: `repo-commit/skills/build/..\escaped`, body: "bad"}},
		{name: "different archive root", entry: githubTestEntry{name: "other-root/skills/build/extra", body: "bad"}},
		{name: "symlink", entry: githubTestEntry{name: "repo-commit/skills/build/link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}},
		{name: "hardlink", entry: githubTestEntry{name: "repo-commit/skills/build/link", typeflag: tar.TypeLink, linkname: "repo-commit/skills/build/SKILL.md"}},
		{name: "fifo", entry: githubTestEntry{name: "repo-commit/skills/build/pipe", typeflag: tar.TypeFifo}},
		{name: "windows device", entry: githubTestEntry{name: "repo-commit/skills/build/NUL.txt", body: "bad"}},
		{name: "trailing dot", entry: githubTestEntry{name: "repo-commit/skills/build/file.", body: "bad"}},
		{name: "alternate data stream", entry: githubTestEntry{name: "repo-commit/skills/build/file:stream", body: "bad"}},
		{name: "case file collision", entry: githubTestEntry{name: "repo-commit/skills/build/skill.md", body: "bad"}},
		{name: "case directory collision", entry: githubTestEntry{name: "repo-commit/skills/Build/SKILL.md", body: githubTestSkill("other", "Case collision")}},
		{name: "duplicate file", entry: base},
		{name: "oversized file", entry: githubTestEntry{name: "repo-commit/skills/build/huge", size: githubFileLimit + 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			archive := githubTestArchive(t, base, test.entry)
			githubTestClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) })
			snapshot := filepath.Join(t.TempDir(), "snapshot")
			err := (githubSource{repository: "owner/repo"}).download(t.Context(), githubTestCommit, snapshot)
			require.Error(t, err)
			_, err = os.Stat(snapshot)
			require.ErrorIs(t, err, os.ErrNotExist, "invalid archives must not publish a snapshot")
			staging, err := filepath.Glob(filepath.Join(filepath.Dir(snapshot), ".download-*"))
			require.NoError(t, err)
			assert.Empty(t, staging, "failed extraction must clean its staging directory")
		})
	}
}

func TestGitHubArchiveLimits(t *testing.T) {
	base := githubTestEntry{name: "repo-commit/skills/build/SKILL.md", body: githubTestSkill("build", "Safe skill")}
	t.Run("file count", func(t *testing.T) {
		const limit = 3
		for _, test := range []struct {
			name  string
			files int
		}{
			{name: "at limit", files: limit},
			{name: "one over limit", files: limit + 1},
		} {
			t.Run(test.name, func(t *testing.T) {
				entries := []githubTestEntry{
					{name: "repo-commit/skills/build/", typeflag: tar.TypeDir},
					{name: "repo-commit/README.md", body: "unselected"},
					base,
				}
				for i := range test.files - 1 {
					entries = append(entries, githubTestEntry{name: fmt.Sprintf("repo-commit/skills/build/file-%04d", i)})
				}
				destination := t.TempDir()
				err := extractGitHubSkillsWithFileLimit(t.Context(), githubTestArchive(t, entries...), []string{"skills/build"}, destination, limit)
				if test.files == limit {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "download limits")
				}

				files, err := os.ReadDir(filepath.Join(destination, "skills", "build"))
				require.NoError(t, err)
				require.Len(t, files, limit)
				content, err := os.ReadFile(filepath.Join(destination, "skills", "build", skillFile))
				require.NoError(t, err)
				assert.Equal(t, base.body, string(content))
				assert.NoFileExists(t, filepath.Join(destination, "README.md"))
				assert.NoFileExists(t, filepath.Join(destination, "skills", "build", "file-0002"))
			})
		}
	})
	t.Run("total selected size", func(t *testing.T) {
		entries := []githubTestEntry{base}
		for i := range githubArchiveLimit / githubFileLimit {
			entries = append(entries, githubTestEntry{name: fmt.Sprintf("repo-commit/skills/build/file-%02d", i), size: githubFileLimit})
		}
		err := extractGitHubSkills(t.Context(), githubTestArchive(t, entries...), []string{"skills/build"}, t.TempDir())
		require.ErrorContains(t, err, "download limits")
	})
	t.Run("unselected expansion bomb", func(t *testing.T) {
		archive := githubTestArchive(t, base, githubTestEntry{name: "repo-commit/unselected/bomb", size: githubExpandedLimit + 1})
		_, err := (githubSource{}).archiveRoots(t.Context(), archive)
		require.ErrorContains(t, err, "expansion limit")
	})
	t.Run("body size boundary", func(t *testing.T) {
		data, err := readGitHubBody(strings.NewReader("1234"), 4)
		require.NoError(t, err)
		assert.Equal(t, []byte("1234"), data)
		_, err = readGitHubBody(strings.NewReader("12345"), 4)
		require.ErrorContains(t, err, "size limit")
	})
}

func TestGitHubInvalidSkillsDoNotPublish(t *testing.T) {
	for _, test := range []struct {
		name    string
		entries []githubTestEntry
	}{
		{name: "no frontmatter", entries: []githubTestEntry{{name: "repo/skills/build/SKILL.md", body: "# Not a skill"}}},
		{name: "unclosed frontmatter", entries: []githubTestEntry{{name: "repo/skills/build/SKILL.md", body: "---\ndescription: Invalid\n"}}},
		{name: "short malformed frontmatter", entries: []githubTestEntry{{name: "repo/skills/build/SKILL.md", body: "---\n---"}}},
		{name: "missing description", entries: []githubTestEntry{{name: "repo/skills/build/SKILL.md", body: "---\nname: build\n---\n"}}},
		{name: "invalid name", entries: []githubTestEntry{{name: "repo/skills/build/SKILL.md", body: githubTestSkill("../escape", "Invalid name")}}},
		{name: "duplicate names", entries: []githubTestEntry{
			{name: "repo/skills/first/SKILL.md", body: githubTestSkill("same", "First")},
			{name: "repo/skills/second/SKILL.md", body: githubTestSkill("same", "Second")},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			archive := githubTestArchive(t, test.entries...)
			githubTestClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) })
			snapshot := filepath.Join(t.TempDir(), "snapshot")
			require.Error(t, (githubSource{repository: "owner/repo"}).download(t.Context(), githubTestCommit, snapshot))
			_, err := os.Stat(snapshot)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestGitHubDownloadFailureCanRetry(t *testing.T) {
	archive := githubTestArchive(t, githubTestEntry{name: "repo/skills/build/SKILL.md", body: githubTestSkill("build", "Build images")})
	fail := true
	var requests []string
	githubTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Host+r.URL.Path)
		switch {
		case r.Host == "api.github.com":
			fmt.Fprint(w, `{"private":false,"visibility":"public","default_branch":"main"}`)
		case fail:
			_, _ = w.Write([]byte("not a gzip archive"))
		default:
			_, _ = w.Write(archive)
		}
	})
	cache := newDiskCache(t.TempDir())
	source := githubSource{repository: "owner/repo", ref: githubTestCommit}
	_, err := loadGitHubSkills(t.Context(), source, cache, nil)
	require.Error(t, err)
	_, err = os.Stat(githubTestSnapshot(cache, source, githubTestCommit))
	require.ErrorIs(t, err, os.ErrNotExist)
	fail = false
	requests = nil
	loaded, err := loadGitHubSkills(t.Context(), source, cache, nil)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, []string{"codeload.github.com/owner/repo/tar.gz/" + githubTestCommit}, requests)
}

func TestGitHubAPIFailuresAndRedirects(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "rate limit", status: http.StatusForbidden, want: "GITHUB_TOKEN"},
		{name: "too many requests", status: http.StatusTooManyRequests, want: "rate limits"},
		{name: "missing repository", status: http.StatusNotFound, want: "HTTP 404"},
		{name: "redirect rejected", status: http.StatusFound, want: "HTTP 302"},
		{name: "malformed JSON", status: http.StatusOK, body: "{", want: "decoding GitHub response"},
		{name: "missing default branch", status: http.StatusOK, body: `{"private":false,"visibility":"public"}`, want: "no default branch"},
		{name: "oversized API response", status: http.StatusOK, body: strings.Repeat(" ", (1<<20)+1), want: "size limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			githubTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.Header().Set("Location", "https://api.github.com/redirected")
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			})
			cache := newDiskCache(t.TempDir())
			_, err := loadGitHubSkills(t.Context(), githubSource{repository: "owner/repo"}, cache, nil)
			require.ErrorContains(t, err, test.want)
			assert.Equal(t, 1, requests, "redirects must never be followed")
			entries, err := os.ReadDir(cache.baseDir)
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}

func TestGitHubConcurrentLoads(t *testing.T) {
	archive := githubTestArchive(t, githubTestEntry{name: "repo/skills/build/SKILL.md", body: githubTestSkill("build", "Build images")})
	var mutex sync.Mutex
	requests := 0
	githubTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		requests++
		mutex.Unlock()
		switch r.Host {
		case "api.github.com":
			fmt.Fprint(w, `{"private":false,"visibility":"public","default_branch":"main"}`)
		case "codeload.github.com":
			_, _ = w.Write(archive)
		}
	})
	cache := newDiskCache(t.TempDir())
	source := githubSource{repository: "owner/repo", ref: githubTestCommit}
	type result struct {
		skills []Skill
		err    error
	}
	results := make(chan result, 12)
	start := make(chan struct{})
	for range cap(results) {
		go func() {
			<-start
			loaded, err := loadGitHubSkills(t.Context(), source, cache, nil)
			results <- result{skills: loaded, err: err}
		}()
	}
	close(start)
	for range cap(results) {
		loaded := <-results
		require.NoError(t, loaded.err)
		require.Len(t, loaded.skills, 1)
	}
	mutex.Lock()
	defer mutex.Unlock()
	assert.Equal(t, 2, requests, "concurrent identical sources share public check and archive download")
}

func TestGitHubCanceledArchiveWalk(t *testing.T) {
	archive := githubTestArchive(t, githubTestEntry{name: "repo/skills/build/SKILL.md", body: githubTestSkill("build", "Build images")})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := (githubSource{}).archiveRoots(ctx, archive)
	require.ErrorIs(t, err, context.Canceled)
	err = extractGitHubSkills(ctx, archive, []string{"skills/build"}, t.TempDir())
	require.ErrorIs(t, err, context.Canceled)
}

func TestGitHubLoadWithWarningsIntegration(t *testing.T) {
	previousCache := paths.GetCacheDir()
	paths.SetCacheDir(t.TempDir())
	t.Cleanup(func() { paths.SetCacheDir(previousCache) })
	archive := githubTestArchive(t,
		githubTestEntry{name: "repo/skills/zeta/SKILL.md", body: githubTestSkill("zeta", "Zeta")},
		githubTestEntry{name: "repo/skills/alpha/SKILL.md", body: githubTestSkill("alpha", "Alpha")},
	)
	requests := 0
	githubTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.Host {
		case "api.github.com":
			assert.Equal(t, "Bearer integration-token", r.Header.Get("Authorization"))
			fmt.Fprint(w, `{"private":false,"visibility":"public"}`)
		case "codeload.github.com":
			assert.Empty(t, r.Header.Get("Authorization"))
			_, _ = w.Write(archive)
		}
	})
	env := environment.NewMapEnvProvider(map[string]string{"GITHUB_TOKEN": "integration-token"})
	valid := "https://github.com/owner/repo?ref=" + githubTestCommit
	invalid := "https://github.com/owner/repo?path=../outside"
	loaded, warnings := LoadWithWarnings(t.Context(), []string{invalid, valid}, env)
	require.Len(t, loaded, 2)
	assert.Equal(t, "alpha", loaded[0].Name)
	assert.Equal(t, "zeta", loaded[1].Name)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], invalid)
	assert.NotContains(t, warnings[0], "integration-token")
	assert.Equal(t, 2, requests)
}

func TestGitHubInvalidCachedRoots(t *testing.T) {
	for _, roots := range []string{`["../outside"]`, `["/absolute"]`, `["skills\\escape"]`, `null`, `[]`, `{}`} {
		t.Run(roots, func(t *testing.T) {
			snapshot := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(snapshot, "roots.json"), []byte(roots), 0o600))
			_, err := (githubSource{repository: "owner/repo"}).readSnapshot(snapshot)
			require.Error(t, err)
		})
	}
}

func TestGitHubBodyReadFailure(t *testing.T) {
	reader := io.MultiReader(strings.NewReader("partial"), githubErrorReader{})
	_, err := readGitHubBody(reader, 100)
	require.ErrorIs(t, err, fs.ErrInvalid)
}

type githubErrorReader struct{}

func (githubErrorReader) Read([]byte) (int, error) { return 0, fs.ErrInvalid }

func TestGitHubInvalidResolvedCommit(t *testing.T) {
	githubTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/owner/repo" {
			fmt.Fprint(w, `{"private":false,"visibility":"public","default_branch":"main"}`)
		} else {
			fmt.Fprint(w, `{"sha":"not-a-full-sha"}`)
		}
	})
	cache := newDiskCache(t.TempDir())
	_, err := loadGitHubSkills(t.Context(), githubSource{repository: "owner/repo"}, cache, nil)
	require.ErrorContains(t, err, "invalid commit SHA")
	_, err = os.Stat(filepath.Join(cache.cacheDir((githubSource{repository: "owner/repo"}).cacheKey(), "github"), "resolution.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestGitHubSelectedDirectoriesHaveSeparateSnapshots(t *testing.T) {
	archive := githubTestArchive(t,
		githubTestEntry{name: "repo/skills/first/SKILL.md", body: githubTestSkill("first", "First selection")},
		githubTestEntry{name: "repo/skills/second/SKILL.md", body: githubTestSkill("second", "Second selection")},
	)
	githubTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "api.github.com" {
			fmt.Fprint(w, `{"private":false,"visibility":"public"}`)
		} else {
			_, _ = w.Write(archive)
		}
	})
	cache := newDiskCache(t.TempDir())
	var selections []Skill
	for _, directory := range []string{"skills/first", "skills/second"} {
		source := githubSource{repository: "owner/repo", ref: githubTestCommit, directory: directory}
		loaded, err := loadGitHubSkills(t.Context(), source, cache, nil)
		require.NoError(t, err)
		require.Len(t, loaded, 1)
		selections = append(selections, loaded[0])
	}
	assert.Equal(t, "first", selections[0].Name)
	assert.Equal(t, "second", selections[1].Name)
	assert.NotEqual(t, selections[0].BaseDir, selections[1].BaseDir)
}

func TestGitHubCanceledWaiterDoesNotCancelActiveLoad(t *testing.T) {
	archive := githubTestArchive(t, githubTestEntry{name: "repo/skills/build/SKILL.md", body: githubTestSkill("build", "Build images")})
	entered := make(chan struct{})
	release := make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	githubTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "api.github.com" {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, `{"private":false,"visibility":"public"}`)
		} else {
			_, _ = w.Write(archive)
		}
	})
	cache := newDiskCache(t.TempDir())
	source := githubSource{repository: "owner/repo", ref: githubTestCommit}
	result := make(chan error, 1)
	go func() {
		_, err := loadGitHubSkills(t.Context(), source, cache, nil)
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first load did not reach GitHub")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := loadGitHubSkills(ctx, source, cache, nil)
	require.ErrorIs(t, err, context.Canceled)
	unblock.Do(func() { close(release) })
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("active load did not finish after releasing GitHub")
	}
}

func TestGitHubArchiveChecksum(t *testing.T) {
	t.Parallel()
	archive := githubTestArchive(t, githubTestEntry{name: "repo/skills/example/SKILL.md", body: githubTestSkill("example", "Example")})
	archive[len(archive)-8] ^= 0xff
	_, err := (githubSource{repository: "owner/repo"}).archiveRoots(t.Context(), archive)
	require.ErrorContains(t, err, "invalid checksum")
}

func TestGitHubArchiveGlobalHeader(t *testing.T) {
	t.Parallel()
	archive := githubTestArchive(t,
		githubTestEntry{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader},
		githubTestEntry{name: "repo/", typeflag: tar.TypeDir},
		githubTestEntry{name: "repo/skills/example/SKILL.md", body: githubTestSkill("example", "Example")},
	)
	roots, err := (githubSource{repository: "owner/repo"}).archiveRoots(t.Context(), archive)
	require.NoError(t, err)
	assert.Equal(t, []string{"skills/example"}, roots)
}
