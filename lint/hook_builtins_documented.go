package main

import (
	"bufio"
	"encoding/json"
	"go/ast"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dgageot/rubocop-go/cop"
)

// HookBuiltinsDocumented keeps the builtin registry, schema help, and hook
// reference synchronized. A builtin omitted from either documentation surface
// still works at runtime but is undiscoverable to users and schema-driven tools.
var HookBuiltinsDocumented = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/HookBuiltinsDocumented",
		Description: "every hook builtin must be listed in the schema and hook reference",
		Severity:    cop.Error,
	},
	Scope: cop.OnlyFile("pkg/hooks/builtins/builtins.go"),
	Run: func(p *cop.Pass) {
		declared, err := declaredBuiltinWireNames(p)
		if err != nil || len(declared) == 0 {
			return
		}

		root := filepath.Clean(filepath.Join(filepath.Dir(p.Filename()), "..", "..", ".."))
		schemaNames, err := hookBuiltinsInSchema(filepath.Join(root, "agent-schema.json"))
		if err != nil {
			p.Reportf(p.File.Name, "read agent-schema.json: %v", err)
			return
		}
		docNames, err := hookBuiltinsInReference(filepath.Join(root, "docs", "configuration", "hooks", "index.md"))
		if err != nil {
			p.Reportf(p.File.Name, "read docs/configuration/hooks/index.md: %v", err)
			return
		}

		var missingSchema, missingDocs []string
		for _, wireName := range declared {
			if !schemaNames[wireName] {
				missingSchema = append(missingSchema, wireName)
			}
			if !docNames[wireName] {
				missingDocs = append(missingDocs, wireName)
			}
		}

		p.ReportMissing(p.File.Name, "agent-schema.json is missing hook builtin(s): %s", missingSchema)
		p.ReportMissing(p.File.Name, "docs/configuration/hooks/index.md is missing hook builtin(s): %s", missingDocs)
	},
}

func declaredBuiltinWireNames(p *cop.Pass) ([]string, error) {
	declared, err := p.DirStringConsts(".", cop.ParseDirOptions{
		SkipTests: true,
		SkipFiles: []string{"builtins.go"},
	}, ast.IsExported)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(declared))
	for _, value := range declared {
		names = append(names, value)
	}
	return names, nil
}

func hookBuiltinsInSchema(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var schema struct {
		Definitions map[string]struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, err
	}

	description := schema.Definitions["HookDefinition"].Properties["type"].Description
	return singleQuotedNames(description), nil
}

func hookBuiltinsInReference(path string) (map[string]bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	names := map[string]bool{}
	scanner := bufio.NewScanner(file)
	inBuiltinsTable := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !inBuiltinsTable {
			inBuiltinsTable = strings.HasPrefix(line, "| Builtin")
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		name, _, ok := strings.Cut(strings.TrimPrefix(line, "| `"), "`")
		if ok {
			names[name] = true
		}
	}
	return names, scanner.Err()
}

var singleQuotedName = regexp.MustCompile(`'([a-z][a-z0-9_]*)' \(`)

func singleQuotedNames(text string) map[string]bool {
	names := map[string]bool{}
	for _, match := range singleQuotedName.FindAllStringSubmatch(text, -1) {
		names[match[1]] = true
	}
	return names
}
