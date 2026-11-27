package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	skillsExtension          = "io.modelcontextprotocol/skills"
	skillsMinProtocolVersion = "2026-07-28"
	directoryMIMEType        = "inode/directory"

	maxSkillsListEntries = 1000
	maxDirectoryEntries  = 1000
)

type skillResource struct {
	URI    string `json:"uri"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

type skillResources struct {
	Present  bool
	Dynamic  bool
	Manifest []skillResource
}

func (r *skillResources) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return errors.New("skill resources must be a manifest array or \"dynamic\"")
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		if s != "dynamic" {
			return fmt.Errorf("invalid skill resources string %q: expected \"dynamic\"", s)
		}
		r.Present = true
		r.Dynamic = true
		r.Manifest = nil
		return nil
	}
	if trimmed[0] == '[' {
		var items []skillResource
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return err
		}
		r.Present = true
		r.Dynamic = false
		r.Manifest = items
		return nil
	}
	return errors.New("skill resources must be a manifest array or \"dynamic\"")
}

func (r skillResources) MarshalJSON() ([]byte, error) {
	if r.Dynamic {
		return []byte(`"dynamic"`), nil
	}
	return json.Marshal(r.Manifest)
}

type skillEntry struct {
	URI         string         `json:"uri"`
	Frontmatter map[string]any `json:"frontmatter"`
	Resources   skillResources `json:"resources"`
	Meta        map[string]any `json:"_meta,omitempty"`
}

type skillListParams struct {
	gomcp.ParamsBase

	Cursor *string `json:"cursor,omitempty"`
}

type skillListResult struct {
	gomcp.ResultBase

	ResultType string       `json:"resultType"`
	Skills     []skillEntry `json:"skills"`
	NextCursor string       `json:"nextCursor,omitempty"`
	TTLMs      int64        `json:"ttlMs"`
	CacheScope string       `json:"cacheScope"`
}

type skillGetParams struct {
	gomcp.ParamsBase

	URI string `json:"uri"`
}

type skillGetResult struct {
	gomcp.ResultBase

	ResultType string     `json:"resultType"`
	Skill      skillEntry `json:"skill"`
	TTLMs      int64      `json:"ttlMs"`
	CacheScope string     `json:"cacheScope"`
}

type directoryReadParams struct {
	gomcp.ParamsBase

	URI    string  `json:"uri"`
	Cursor *string `json:"cursor,omitempty"`
}

type directoryReadResult struct {
	gomcp.ResultBase

	ResultType string            `json:"resultType"`
	Resources  []*gomcp.Resource `json:"resources"`
	NextCursor string            `json:"nextCursor,omitempty"`
	TTLMs      int64             `json:"ttlMs"`
	CacheScope string            `json:"cacheScope"`
}

func registerSkillMethods(client *gomcp.Client) error {
	if err := gomcp.AddSendingCustomMethod[*skillListParams, *skillListResult](client, "skills/list"); err != nil {
		return err
	}
	if err := gomcp.AddSendingCustomMethod[*skillGetParams, *skillGetResult](client, "skills/get"); err != nil {
		return err
	}
	return gomcp.AddSendingCustomMethod[*directoryReadParams, *directoryReadResult](client, "resources/directory/read")
}

func parseSkillsExtension(initResult *gomcp.InitializeResult) (supported, directoryRead bool) {
	if initResult == nil || initResult.ProtocolVersion < skillsMinProtocolVersion {
		return false, false
	}
	if initResult.Capabilities == nil || initResult.Capabilities.Resources == nil {
		return false, false
	}
	ext, ok := initResult.Capabilities.Extensions[skillsExtension]
	if !ok {
		return false, false
	}
	if cfg, ok := ext.(map[string]any); ok {
		if v, ok := cfg["directoryRead"].(bool); ok {
			directoryRead = v
		}
	}
	return true, directoryRead
}

// skillRPC speaks the SEP-2640 wire protocol over an initialized MCP session.
type skillRPC struct {
	session  *gomcp.ClientSession
	sanitize func(error) error
}

func (r skillRPC) safeError(err error) error {
	if err != nil && r.sanitize != nil {
		return r.sanitize(err)
	}
	return err
}

func (r skillRPC) listSkills(ctx context.Context) ([]skillEntry, time.Duration, error) {
	var all []skillEntry
	var cursor *string
	var minTTL time.Duration
	hasTTL := false
	allPublic := true
	seen := make(map[string]bool)
	for {
		result, err := gomcp.CallCustomMethod[*skillListParams, *skillListResult](ctx, r.session, "skills/list", &skillListParams{Cursor: cursor})
		if err != nil {
			return nil, 0, r.safeError(err)
		}
		if result.CacheScope != "public" {
			allPublic = false
		}
		pageTTL := max(time.Duration(result.TTLMs)*time.Millisecond, 0)
		if !hasTTL || pageTTL < minTTL {
			minTTL = pageTTL
			hasTTL = true
		}
		all = append(all, result.Skills...)
		if len(all) > maxSkillsListEntries {
			return nil, 0, fmt.Errorf("MCP skills listing exceeded %d entries", maxSkillsListEntries)
		}
		if result.NextCursor == "" {
			if !allPublic {
				return all, 0, nil
			}
			return all, minTTL, nil
		}
		if seen[result.NextCursor] {
			return nil, 0, errors.New("MCP skills listing repeated a pagination cursor")
		}
		seen[result.NextCursor] = true
		cursor = &result.NextCursor
	}
}

func (r skillRPC) getSkill(ctx context.Context, uri string) (skillEntry, error) {
	result, err := gomcp.CallCustomMethod[*skillGetParams, *skillGetResult](ctx, r.session, "skills/get", &skillGetParams{URI: uri})
	if err != nil {
		return skillEntry{}, r.safeError(err)
	}
	return result.Skill, nil
}

func (r skillRPC) readDirectory(ctx context.Context, dirURI string) ([]*gomcp.Resource, error) {
	var all []*gomcp.Resource
	var cursor *string
	seen := make(map[string]bool)
	for {
		result, err := gomcp.CallCustomMethod[*directoryReadParams, *directoryReadResult](ctx, r.session, "resources/directory/read", &directoryReadParams{URI: dirURI, Cursor: cursor})
		if err != nil {
			return nil, r.safeError(err)
		}
		all = append(all, result.Resources...)
		if len(all) > maxDirectoryEntries {
			return nil, fmt.Errorf("MCP directory listing exceeded %d entries", maxDirectoryEntries)
		}
		if result.NextCursor == "" {
			return all, nil
		}
		if seen[result.NextCursor] {
			return nil, errors.New("MCP directory listing repeated a pagination cursor")
		}
		seen[result.NextCursor] = true
		cursor = &result.NextCursor
	}
}

func (r skillRPC) readResource(ctx context.Context, uri string) (raw []byte, mimeType string, isBinary bool, err error) {
	result, err := r.session.ReadResource(ctx, &gomcp.ReadResourceParams{URI: uri})
	if err != nil {
		return nil, "", false, r.safeError(err)
	}
	if result == nil || len(result.Contents) == 0 {
		return nil, "", false, fmt.Errorf("empty resources/read result for %q", uri)
	}
	var rc *gomcp.ResourceContents
	for _, item := range result.Contents {
		if item != nil && item.URI == uri {
			rc = item
			break
		}
	}
	if rc == nil {
		return nil, "", false, fmt.Errorf("resources/read response did not contain %q", uri)
	}
	if len(rc.Blob) > 0 {
		return rc.Blob, rc.MIMEType, true, nil
	}
	return []byte(rc.Text), rc.MIMEType, false, nil
}
