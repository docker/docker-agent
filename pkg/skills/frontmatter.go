package skills

import "strings"

// parseFrontmatter extracts and parses the YAML-like frontmatter from a
// markdown file. Instead of using a full YAML parser (which rejects unquoted
// colons in values), we do simple line-by-line key: value splitting on the
// first ": ". This is more robust for the simple frontmatter format used by
// skill files.
func parseFrontmatter(content string) (Skill, bool) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	if !strings.HasPrefix(content, "---") {
		return Skill{}, false
	}

	endIndex := strings.Index(content[3:], "\n---")
	if endIndex == -1 {
		return Skill{}, false
	}

	block := content[4 : endIndex+3]

	var skill Skill
	var currentKey string // tracks multi-line keys like "metadata" or "allowed-tools"

	for line := range strings.SplitSeq(block, "\n") {
		// Indented lines belong to the current multi-line key.
		if line != "" && (line[0] == ' ' || line[0] == '\t') {
			parseIndentedLine(&skill, currentKey, strings.TrimSpace(line))
			continue
		}

		currentKey = ""
		key, value, ok := splitKeyValue(line)
		if !ok {
			continue
		}

		if multi := parseTopLevelLine(&skill, key, value); multi {
			currentKey = key
		}
	}

	return skill, true
}

// parseTopLevelLine assigns a frontmatter key/value to the right Skill field.
// It returns true if the key opens a multi-line block whose subsequent
// indented lines must be collected by parseIndentedLine.
func parseTopLevelLine(skill *Skill, key, value string) bool {
	switch key {
	case "name":
		skill.Name = unquote(value)
	case "description":
		skill.Description = unquote(value)
	case "license":
		skill.License = unquote(value)
	case "compatibility":
		skill.Compatibility = unquote(value)
	case "context":
		skill.Context = unquote(value)
	case "model":
		skill.Model = unquote(value)
	case "metadata":
		// metadata is always multi-line.
		return true
	case "allowed-tools":
		if value == "" {
			// Block form: subsequent indented "- item" lines.
			return true
		}
		// Inline comma-separated list.
		for item := range strings.SplitSeq(value, ",") {
			if t := unquote(strings.TrimSpace(item)); t != "" {
				skill.AllowedTools = append(skill.AllowedTools, t)
			}
		}
	}
	return false
}

// parseIndentedLine accumulates an indented child line into the multi-line
// block identified by parentKey.
func parseIndentedLine(skill *Skill, parentKey, line string) {
	switch parentKey {
	case "metadata":
		if k, v, ok := splitKeyValue(line); ok {
			if skill.Metadata == nil {
				skill.Metadata = make(map[string]string)
			}
			skill.Metadata[k] = unquote(v)
		}
	case "allowed-tools":
		if item, ok := strings.CutPrefix(line, "- "); ok {
			skill.AllowedTools = append(skill.AllowedTools, unquote(strings.TrimSpace(item)))
		}
	}
}

// splitKeyValue splits a line on the first ": " into key and value.
// It also handles bare "key:" (no value) for keys that introduce a block.
func splitKeyValue(line string) (string, string, bool) {
	if key, value, ok := strings.Cut(line, ": "); ok {
		return key, value, true
	}
	if strings.HasSuffix(line, ":") {
		return line[:len(line)-1], "", true
	}
	return "", "", false
}

// unquote strips matching surrounding single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
