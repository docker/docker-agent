package skills

import (
	"context"
	"net/url"
	"strings"
)

const mcpSkillRefPrefix = "mcp:"

// MCPSkillSummary describes an Agent Skill advertised by a connected MCP server
// implementing the io.modelcontextprotocol/skills extension (SEP-2640).
type MCPSkillSummary struct {
	URI         string
	Name        string
	Description string
	Files       []string // Relative supporting file paths (excluding SKILL.md).
	Dynamic     bool
}

// MCPSkillProvider is implemented by MCP toolsets that can discover and serve
// verified Agent Skills over MCP.
type MCPSkillProvider interface {
	// SkillOrigin returns the host-assigned label identifying this MCP server
	// (never the server's self-reported serverInfo.name).
	SkillOrigin() string
	// SupportsMCPSkills reports whether this server is connected, opted in,
	// and declared both resources and io.modelcontextprotocol/skills.
	SupportsMCPSkills() bool
	// ListMCPSkills returns the server's validated skill entries without
	// fetching SKILL.md or supporting file bodies.
	ListMCPSkills(ctx context.Context) ([]MCPSkillSummary, error)
	// ReadMCPSkill loads and verifies SKILL.md by URI (via skills/get and
	// resources/read), holds its manifest for the acting window, and returns
	// origin-tagged content.
	ReadMCPSkill(ctx context.Context, uri string) (string, error)
	// ReadMCPSkillFile reads and verifies a supporting file (or lists a
	// directory when path is empty, ".", or ends with "/") of a loaded skill.
	ReadMCPSkillFile(ctx context.Context, skillURI, relPath string) (string, error)
}

// FormatMCPSkillRef returns the origin-scoped skill reference for an MCP skill.
func FormatMCPSkillRef(origin, uri string) string {
	return mcpSkillRefPrefix + url.PathEscape(origin) + "/" + uri
}

// ParseMCPSkillRef parses an origin-scoped MCP skill reference of the form
// "mcp:<origin>/<uri>".
func ParseMCPSkillRef(ref string) (origin, uri string, ok bool) {
	rest, hasPrefix := strings.CutPrefix(ref, mcpSkillRefPrefix)
	if !hasPrefix {
		return "", "", false
	}
	rawOrigin, uri, found := strings.Cut(rest, "/")
	if !found || rawOrigin == "" || uri == "" || !strings.Contains(uri, "://") {
		return "", "", false
	}
	origin, err := url.PathUnescape(rawOrigin)
	if err != nil || origin == "" {
		return "", "", false
	}
	return origin, uri, true
}
