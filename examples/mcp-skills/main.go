package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	skillsExtension = "io.modelcontextprotocol/skills"
	skillMDURI      = "skill://code-review/SKILL.md"
	checklistURI    = "skill://code-review/references/checklist.md"
)

const (
	skillMDContent = "---\nname: code-review\ndescription: Review code using the team's checklist.\n---\n\n# Code review\n\nRead `references/checklist.md`, then review the diff.\n"
	checklistBody  = "# Review checklist\n\nCheck correctness, tests, and compatibility.\n"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func sha256Digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func main() {
	files := map[string]string{
		skillMDURI:   skillMDContent,
		checklistURI: checklistBody,
	}

	entry := map[string]any{
		"uri": skillMDURI,
		"frontmatter": map[string]any{
			"name":        "code-review",
			"description": "Review code using the team's checklist.",
		},
		"resources": []map[string]any{
			{"uri": skillMDURI, "digest": sha256Digest(skillMDContent), "size": len(skillMDContent)},
			{"uri": checklistURI, "digest": sha256Digest(checklistBody), "size": len(checklistBody)},
		},
	}

	dirs := map[string][]map[string]any{
		"skill://code-review": {
			{"uri": skillMDURI, "name": "SKILL.md", "mimeType": "text/markdown"},
			{"uri": "skill://code-review/references", "name": "references", "mimeType": "inode/directory"},
		},
		"skill://code-review/references": {
			{"uri": checklistURI, "name": "checklist.md", "mimeType": "text/markdown"},
		},
	}

	enc := json.NewEncoder(os.Stdout)
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF && len(line) == 0 {
				return
			}
			if len(line) == 0 {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(trimmed), &req); err != nil {
			continue
		}
		if len(req.ID) == 0 {
			continue
		}

		reply := func(res any) {
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
		}
		replyErr := func(code int, msg string) {
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": code, "message": msg}})
		}

		switch req.Method {
		case "initialize":
			reply(map[string]any{
				"protocolVersion": "2026-07-28",
				"capabilities": map[string]any{
					"resources": map[string]any{},
					"extensions": map[string]any{
						skillsExtension: map[string]any{"directoryRead": true},
					},
				},
				"serverInfo": map[string]any{"name": "demo-skills-server", "version": "1.0.0"},
			})
		case "tools/list":
			reply(map[string]any{"tools": []any{}})
		case "skills/list":
			reply(map[string]any{
				"resultType": "complete",
				"skills":     []any{entry},
				"ttlMs":      300000,
				"cacheScope": "public",
			})
		case "skills/get":
			var p struct {
				URI string `json:"uri"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.URI != skillMDURI {
				replyErr(-32602, "unknown skill: "+p.URI)
				continue
			}
			reply(map[string]any{
				"resultType": "complete",
				"skill":      entry,
				"ttlMs":      300000,
				"cacheScope": "public",
			})
		case "resources/read":
			var p struct {
				URI string `json:"uri"`
			}
			_ = json.Unmarshal(req.Params, &p)
			body, ok := files[p.URI]
			if !ok {
				replyErr(-32602, "unknown resource: "+p.URI)
				continue
			}
			reply(map[string]any{
				"resultType": "complete",
				"contents": []map[string]any{
					{"uri": p.URI, "mimeType": "text/markdown", "text": body},
				},
				"ttlMs":      300000,
				"cacheScope": "public",
			})
		case "resources/directory/read":
			var p struct {
				URI string `json:"uri"`
			}
			_ = json.Unmarshal(req.Params, &p)
			children, ok := dirs[p.URI]
			if !ok {
				replyErr(-32602, "not a directory: "+p.URI)
				continue
			}
			reply(map[string]any{
				"resultType": "complete",
				"resources":  children,
				"ttlMs":      300000,
				"cacheScope": "public",
			})
		default:
			replyErr(-32601, "method not found: "+req.Method)
		}
	}
}
