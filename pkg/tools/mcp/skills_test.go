package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestCreateToolSetExperimentalSkills(t *testing.T) {
	for _, tc := range []struct {
		name         string
		experimental *userconfig.ExperimentalSettings
		want         bool
	}{
		{name: "unset", experimental: nil, want: false},
		{name: "disabled", experimental: &userconfig.ExperimentalSettings{}, want: false},
		{name: "enabled", experimental: &userconfig.ExperimentalSettings{MCPSkills: true}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths.SetConfigDir(t.TempDir())
			t.Cleanup(func() { paths.SetConfigDir("") })
			if tc.experimental != nil {
				require.NoError(t, (&userconfig.Config{
					Settings: &userconfig.Settings{Experimental: tc.experimental},
				}).Save())
			}

			runConfig := &config.RuntimeConfig{EnvProviderOverride: environment.NewMapEnvProvider(nil)}
			cfg := latest.Toolset{Type: "mcp", Remote: latest.Remote{URL: "https://example.com/mcp", TransportType: "streamable"}}
			toolset, err := CreateToolSet(t.Context(), cfg, runConfig)
			require.NoError(t, err)
			assert.Equal(t, tc.want, toolset.(*Toolset).mcpClient.(*remoteMCPClient).skillsEnabled())

			fromConfig := NewRemoteToolsetFromConfig(t.Context(), cfg, runConfig.EnvProvider())
			assert.Equal(t, tc.want, fromConfig.mcpClient.(*remoteMCPClient).skillsEnabled())
		})
	}
}

func TestValidatedSkill(t *testing.T) {
	t.Parallel()

	skillMD := []byte("---\nname: code-review\ndescription: Review code\n---\n# Body\n")
	makeEntry := func(uri string) skillEntry {
		return skillEntry{
			URI:         uri,
			Frontmatter: map[string]any{"name": "code-review", "description": "Review code"},
			Resources: skillResources{
				Present:  true,
				Manifest: []skillResource{{URI: uri, Digest: sha256Digest(skillMD), Size: int64(len(skillMD))}},
			},
		}
	}

	// Valid schemes: skill://, custom scheme, and empty-authority file:///.
	for _, uri := range []string{
		"skill://acme/code-review/SKILL.md",
		"github://owner/repo/code-review/SKILL.md",
		"file:///skills/code-review/SKILL.md",
	} {
		vs, err := newValidatedSkill(makeEntry(uri))
		require.NoError(t, err, uri)
		assert.Equal(t, "code-review", vs.name)
	}

	valid := makeEntry("skill://acme/code-review/SKILL.md")
	tooMany := make([]skillResource, maxSkillResources+1)
	tooMany[0] = valid.Resources.Manifest[0]
	for i := 1; i < len(tooMany); i++ {
		tooMany[i] = skillResource{URI: fmt.Sprintf("skill://acme/code-review/f%d.txt", i), Digest: sha256Digest([]byte("a")), Size: 1}
	}

	invalidCases := []struct {
		name    string
		mutate  func(*skillEntry)
		wantErr string
	}{
		{"name mismatch", func(e *skillEntry) { e.Frontmatter = map[string]any{"name": "other", "description": "x"} }, "does not match frontmatter name"},
		{"missing resources", func(e *skillEntry) { e.Resources = skillResources{} }, "missing resources"},
		{"missing SKILL.md", func(e *skillEntry) {
			e.Resources.Manifest = []skillResource{{URI: "skill://acme/code-review/ref.md", Digest: sha256Digest([]byte("x")), Size: 1}}
		}, "missing SKILL.md"},
		{"escapes root", func(e *skillEntry) {
			e.Resources.Manifest = append(e.Resources.Manifest, skillResource{URI: "skill://other/secret.md", Digest: sha256Digest([]byte("x")), Size: 1})
		}, "escapes skill root"},
		{"encoded traversal", func(e *skillEntry) {
			e.Resources.Manifest = append(e.Resources.Manifest, skillResource{URI: "skill://acme/code-review/%2e%2e/secret.md", Digest: sha256Digest([]byte("x")), Size: 1})
		}, "invalid relative path"},
		{"exceeds 512 files", func(e *skillEntry) { e.Resources.Manifest = tooMany }, "exceeding limit"},
		{"exceeds 16 MiB", func(e *skillEntry) { e.Resources.Manifest[0].Size = maxSkillTotalBytes + 1 }, "total size exceeds limit"},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			e := makeEntry(valid.URI)
			tc.mutate(&e)
			_, err := newValidatedSkill(e)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}

	vs, err := newValidatedSkill(valid)
	require.NoError(t, err)
	_, err = vs.resolveFileURI("%2e%2e/secret.md")
	require.Error(t, err)
	_, err = vs.resolveDirURI("refs/%2e%2e")
	require.Error(t, err)
}

type mockSkillsServer struct {
	mu        sync.Mutex
	skills    map[string]skillEntry
	listOrder []string
	files     map[string][]byte
	dirs      map[string][]map[string]any
	readCount atomic.Int64
	listCount atomic.Int64
}

func (m *mockSkillsServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		writeJSON := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		}
		reply := func(res any) { writeJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res}) }
		replyErr := func(code int, msg string) {
			writeJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": code, "message": msg}})
		}

		m.mu.Lock()
		defer m.mu.Unlock()

		switch req.Method {
		case "initialize":
			reply(map[string]any{
				"protocolVersion": "2026-07-28",
				"capabilities": map[string]any{
					"resources":  map[string]any{},
					"extensions": map[string]any{skillsExtension: map[string]any{"directoryRead": true}},
				},
				"serverInfo": map[string]any{"name": "untrusted-server-name", "version": "1.0.0"},
			})
		case "tools/list":
			replyErr(-32601, "method not found")
		case "skills/list":
			m.listCount.Add(1)
			var p struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(req.Params, &p)
			idx := 0
			if p.Cursor == "page2" {
				idx = 1
			}
			res := map[string]any{"resultType": "complete", "skills": []skillEntry{m.skills[m.listOrder[idx]]}, "ttlMs": 300000, "cacheScope": "public"}
			if idx+1 < len(m.listOrder) {
				res["nextCursor"] = "page2"
			}
			reply(res)
		case "skills/get":
			var p struct {
				URI string `json:"uri"`
			}
			_ = json.Unmarshal(req.Params, &p)
			entry, ok := m.skills[p.URI]
			if !ok {
				replyErr(-32602, "unknown skill")
				return
			}
			reply(map[string]any{"resultType": "complete", "skill": entry, "ttlMs": 300000, "cacheScope": "public"})
		case "resources/read":
			m.readCount.Add(1)
			var p struct {
				URI string `json:"uri"`
			}
			_ = json.Unmarshal(req.Params, &p)
			data, ok := m.files[p.URI]
			if !ok {
				replyErr(-32602, "unknown resource")
				return
			}
			reply(map[string]any{
				"resultType": "complete",
				"contents":   []map[string]any{{"uri": p.URI, "mimeType": "text/markdown", "text": string(data)}},
				"ttlMs":      300000,
				"cacheScope": "public",
			})
		case "resources/directory/read":
			var p struct {
				URI string `json:"uri"`
			}
			_ = json.Unmarshal(req.Params, &p)
			entries, ok := m.dirs[p.URI]
			if !ok {
				replyErr(-32602, "not a directory")
				return
			}
			reply(map[string]any{"resultType": "complete", "resources": entries, "ttlMs": 300000, "cacheScope": "public"})
		default:
			replyErr(-32601, "method not found: "+req.Method)
		}
	})
}

func TestMCPSkillsEndToEnd(t *testing.T) {
	t.Parallel()

	reviewMD := []byte("---\nname: code-review\ndescription: Review code using the team's checklist.\n---\n\n# Code review\n")
	checklistMD := []byte("# Review checklist\n\nCheck correctness and tests.\n")
	unlistedMD := []byte("---\nname: unlisted\ndescription: Direct URI skill\n---\n\n# Unlisted\n")
	dynamicMD := []byte("---\nname: daily\ndescription: Dynamic report\n---\n\n# Daily\n")
	tamperedMD := []byte("---\nname: bad\ndescription: Evil\n---\n# Body\n")
	extraFrontmatterMD := []byte("---\nname: badfm\ndescription: Good\nallowed-tools: shell\n---\n# Body\n")

	mock := &mockSkillsServer{
		listOrder: []string{"skill://code-review/SKILL.md", "skill://reports/daily/SKILL.md"},
		skills: map[string]skillEntry{
			"skill://code-review/SKILL.md": {
				URI:         "skill://code-review/SKILL.md",
				Frontmatter: map[string]any{"name": "code-review", "description": "Review code using the team's checklist."},
				Resources: skillResources{
					Present: true,
					Manifest: []skillResource{
						{URI: "skill://code-review/SKILL.md", Digest: sha256Digest(reviewMD), Size: int64(len(reviewMD))},
						{URI: "skill://code-review/references/checklist.md", Digest: sha256Digest(checklistMD), Size: int64(len(checklistMD))},
					},
				},
			},
			"skill://unlisted/SKILL.md": {
				URI:         "skill://unlisted/SKILL.md",
				Frontmatter: map[string]any{"name": "unlisted", "description": "Direct URI skill"},
				Resources:   skillResources{Present: true, Manifest: []skillResource{{URI: "skill://unlisted/SKILL.md", Digest: sha256Digest(unlistedMD), Size: int64(len(unlistedMD))}}},
			},
			"skill://reports/daily/SKILL.md": {
				URI:         "skill://reports/daily/SKILL.md",
				Frontmatter: map[string]any{"name": "daily", "description": "Dynamic report"},
				Resources:   skillResources{Present: true, Dynamic: true},
			},
			"skill://bad/SKILL.md": {
				URI:         "skill://bad/SKILL.md",
				Frontmatter: map[string]any{"name": "bad", "description": "Good"},
				Resources:   skillResources{Present: true, Manifest: []skillResource{{URI: "skill://bad/SKILL.md", Digest: sha256Digest(reviewMD), Size: int64(len(tamperedMD))}}},
			},
			"skill://badfm/SKILL.md": {
				URI:         "skill://badfm/SKILL.md",
				Frontmatter: map[string]any{"name": "badfm", "description": "Good"},
				Resources:   skillResources{Present: true, Manifest: []skillResource{{URI: "skill://badfm/SKILL.md", Digest: sha256Digest(extraFrontmatterMD), Size: int64(len(extraFrontmatterMD))}}},
			},
		},
		files: map[string][]byte{
			"skill://code-review/SKILL.md":                reviewMD,
			"skill://code-review/references/checklist.md": checklistMD,
			"skill://code-review/references/unlisted.md":  []byte("not in manifest\n"),
			"skill://unlisted/SKILL.md":                   unlistedMD,
			"skill://reports/daily/SKILL.md":              dynamicMD,
			"skill://reports/daily/live.md":               []byte("Live data\n"),
			"skill://bad/SKILL.md":                        tamperedMD,
			"skill://badfm/SKILL.md":                      extraFrontmatterMD,
		},
		dirs: map[string][]map[string]any{
			"skill://reports/daily": {
				{"uri": "skill://reports/daily/live.md", "name": "live.md", "mimeType": "text/markdown"},
			},
		},
	}

	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	// Disabled by default: skills-only server does not advertise support.
	disabledTS := NewRemoteToolsetWithAllowPrivateIPs("docs", srv.URL, "streamable", nil, nil, true, lifecycle.Policy{})
	require.NoError(t, disabledTS.Start(t.Context()))
	assert.False(t, disabledTS.SupportsMCPSkills())
	_, err := disabledTS.Tools(t.Context())
	require.Error(t, err)
	require.NoError(t, disabledTS.Stop(t.Context()))

	// Enabled: skills-only server exposes 0 synthetic MCP tools and serves skills via provider interface.
	ts := NewRemoteToolsetWithAllowPrivateIPs("docs", srv.URL, "streamable", nil, nil, true, lifecycle.Policy{})
	ts.SetExperimentalSkills(true)
	require.NoError(t, ts.Start(t.Context()))
	defer func() { require.NoError(t, ts.Stop(t.Context())) }()

	assert.True(t, ts.SupportsMCPSkills())
	assert.Equal(t, "docs", ts.SkillOrigin())
	toolList, err := ts.Tools(t.Context())
	require.NoError(t, err)
	assert.Empty(t, toolList)

	// Paginated listing is lazy (no file reads) and cached for ttlMs.
	listed, err := ts.ListMCPSkills(t.Context())
	require.NoError(t, err)
	assert.Zero(t, mock.readCount.Load())
	require.Len(t, listed, 2)
	assert.Equal(t, []string{"references/checklist.md"}, listed[0].Files)
	assert.True(t, listed[1].Dynamic)

	listed2, err := ts.ListMCPSkills(t.Context())
	require.NoError(t, err)
	assert.Equal(t, listed, listed2)
	assert.Equal(t, int64(2), mock.listCount.Load())

	sc := ts.mcpClient.(*remoteMCPClient).skills
	sc.mu.Lock()
	sc.now = func() time.Time { return time.Now().Add(6 * time.Minute) }
	sc.mu.Unlock()
	_, err = ts.ListMCPSkills(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(4), mock.listCount.Load())

	// Supporting files require loading the skill first, verify against manifest, and hit cache.
	_, err = ts.ReadMCPSkillFile(t.Context(), "skill://code-review/SKILL.md", "references/checklist.md")
	require.ErrorContains(t, err, "not loaded")

	out, err := ts.ReadMCPSkill(t.Context(), "skill://code-review/SKILL.md")
	require.NoError(t, err)
	assert.Contains(t, out, `server "docs"`)
	assert.Contains(t, out, "Untrusted external MCP content")

	refOut, err := ts.ReadMCPSkillFile(t.Context(), "skill://code-review/SKILL.md", "references/checklist.md")
	require.NoError(t, err)
	assert.Contains(t, refOut, "Check correctness and tests.")
	readsBeforeCache := mock.readCount.Load()
	refOut2, err := ts.ReadMCPSkillFile(t.Context(), "skill://code-review/SKILL.md", "skill://code-review/references/checklist.md")
	require.NoError(t, err)
	assert.Equal(t, refOut, refOut2)
	assert.Equal(t, readsBeforeCache, mock.readCount.Load())

	// Unlisted file, traversal, digest mismatch, and frontmatter mismatch are all rejected.
	_, err = ts.ReadMCPSkillFile(t.Context(), "skill://code-review/SKILL.md", "references/unlisted.md")
	require.ErrorContains(t, err, "not in the held manifest")
	_, err = ts.ReadMCPSkillFile(t.Context(), "skill://code-review/SKILL.md", "../unlisted/SKILL.md")
	require.ErrorContains(t, err, "escapes skill root")
	_, err = ts.ReadMCPSkill(t.Context(), "skill://bad/SKILL.md")
	require.ErrorContains(t, err, "digest mismatch")
	_, err = ts.ReadMCPSkill(t.Context(), "skill://badfm/SKILL.md")
	require.ErrorContains(t, err, "frontmatter does not match")

	// Manifest directory listing and conversation-scoped acting windows.
	dirOut, err := ts.ReadMCPSkillFile(t.Context(), "skill://code-review/SKILL.md", "references/")
	require.NoError(t, err)
	assert.Contains(t, dirOut, "checklist.md")
	assert.NotContains(t, dirOut, "unlisted.md")

	ctxA := httpclient.ContextWithSessionID(t.Context(), "session-a")
	ctxB := httpclient.ContextWithSessionID(t.Context(), "session-b")
	_, err = ts.ReadMCPSkill(ctxA, "skill://code-review/SKILL.md")
	require.NoError(t, err)
	_, err = ts.ReadMCPSkillFile(ctxB, "skill://code-review/SKILL.md", "references/checklist.md")
	require.ErrorContains(t, err, "not loaded")

	// Unlisted direct URI and dynamic skill with directoryRead.
	unlistedOut, err := ts.ReadMCPSkill(t.Context(), "skill://unlisted/SKILL.md")
	require.NoError(t, err)
	assert.Contains(t, unlistedOut, "# Unlisted")

	dynOut, err := ts.ReadMCPSkill(t.Context(), "skill://reports/daily/SKILL.md")
	require.NoError(t, err)
	assert.Contains(t, dynOut, "resources: dynamic")
	dynDirOut, err := ts.ReadMCPSkillFile(t.Context(), "skill://reports/daily/SKILL.md", ".")
	require.NoError(t, err)
	assert.Contains(t, dynDirOut, "live.md")
	dynFileOut, err := ts.ReadMCPSkillFile(t.Context(), "skill://reports/daily/SKILL.md", "live.md")
	require.NoError(t, err)
	assert.Contains(t, dynFileOut, "Live data")
}

func TestMCPSkillsStdioDemoEndToEnd(t *testing.T) {
	t.Parallel()

	ts := NewToolsetCommand("demo", "go", []string{"run", "../../../examples/mcp-skills"}, nil, "")
	ts.SetExperimentalSkills(true)
	require.NoError(t, ts.Start(t.Context()))
	defer func() { require.NoError(t, ts.Stop(t.Context())) }()

	listed, err := ts.ListMCPSkills(t.Context())
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "skill://code-review/SKILL.md", listed[0].URI)

	readOut, err := ts.ReadMCPSkill(t.Context(), "skill://code-review/SKILL.md")
	require.NoError(t, err)
	assert.Contains(t, readOut, "Read `references/checklist.md`")

	fileOut, err := ts.ReadMCPSkillFile(t.Context(), "skill://code-review/SKILL.md", "references/checklist.md")
	require.NoError(t, err)
	assert.Contains(t, fileOut, "Check correctness, tests, and compatibility.")
}
