// Command gen generates the TypeScript SDK's types and JSON Schemas from the
// Go types the docker-agent API serves and accepts.
//
// Run it from the repository root with `task generate-ts`.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

func main() {
	dir := flag.String("dir", "sdk/typescript", "TypeScript package directory")
	flag.Parse()

	if err := run(context.Background(), *dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dir string) error {
	workDir, err := os.MkdirTemp("", "docker-agent-ts-sdk-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	out, err := generate(ctx, defaultConfig(), workDir)
	if err != nil {
		return err
	}
	if err := writeDir(filepath.Join(dir, "src", "generated"), ".ts", out.sources); err != nil {
		return err
	}
	return writeDir(filepath.Join(dir, "schemas"), ".json", out.schemas)
}

// output maps file names to contents.
type output struct {
	sources map[string]string
	schemas map[string]string
}

// generate builds toolsets in workDir to read their tools.
func generate(ctx context.Context, cfg *config, workDir string) (*output, error) {
	src, err := loadGoSource(sortedKeys(cfg.modules))
	if err != nil {
		return nil, err
	}
	defs, err := build(cfg, src)
	if err != nil {
		return nil, err
	}
	toolsets, err := collectToolsets(ctx, cfg.toolsets, workDir)
	if err != nil {
		return nil, err
	}
	args, err := uniqueToolArgs(toolsets)
	if err != nil {
		return nil, err
	}

	types, err := emitTypeScript(defs)
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for _, d := range defs {
		taken[d.name] = true
	}
	zodFiles, err := emitZod(toolsets, args, taken)
	if err != nil {
		return nil, err
	}
	sources := map[string]string{}
	for _, files := range []map[string]string{types, zodFiles} {
		if err := addFiles(sources, files); err != nil {
			return nil, err
		}
	}
	if err := addFiles(sources, map[string]string{"index.ts": emitIndex(sources)}); err != nil {
		return nil, err
	}

	schema, err := emitJSONSchema(defs)
	if err != nil {
		return nil, err
	}
	toolSchema, err := emitToolArgsJSONSchema(args)
	if err != nil {
		return nil, err
	}
	return &output{
		sources: sources,
		schemas: map[string]string{"docker-agent.schema.json": schema, "tool-args.schema.json": toolSchema},
	}, nil
}

// build returns every def reachable from the configured roots and events,
// plus the event union, sorted by name.
func build(cfg *config, src *goSource) ([]*def, error) {
	b := newBuilder(cfg, src)
	for _, t := range cfg.roots {
		if _, err := b.def(t); err != nil {
			return nil, err
		}
	}
	var union *def
	if len(cfg.events) > 0 {
		var err error
		if union, err = b.eventUnion(cfg.events); err != nil {
			return nil, err
		}
	}
	defs := slices.Concat(slices.Collect(maps.Values(b.defs)), b.extra)
	if union != nil {
		defs = append(defs, union)
	}
	slices.SortFunc(defs, func(a, b *def) int { return cmp.Compare(a.name, b.name) })
	return defs, nil
}

// eventUnion builds a def per event, its "type" narrowed to the event's
// discriminator, and returns their union.
func (b *builder) eventUnion(events map[string]reflect.Type) (*def, error) {
	if _, taken := b.names[unionName]; taken {
		return nil, fmt.Errorf("%s is already the name of a Go type", unionName)
	}
	union := &def{name: unionName, schema: &jsonschema.Schema{Description: "Any event streamed by a session, discriminated by `type`."}}
	for _, key := range sortedKeys(events) {
		d, err := b.def(events[key])
		if err != nil {
			return nil, err
		}
		discriminator, ok := d.schema.Properties["type"]
		if !ok || discriminator.Type != "string" {
			return nil, fmt.Errorf("event %s has no string \"type\" field", d.name)
		}
		d.schema.Properties["type"] = &jsonschema.Schema{Const: new(any(key)), Description: discriminator.Description}
		union.module = cmp.Or(union.module, d.module)
		union.schema.OneOf = append(union.schema.OneOf, &jsonschema.Schema{Ref: "#/$defs/" + d.name})
	}
	slices.SortFunc(union.schema.OneOf, func(a, b *jsonschema.Schema) int { return cmp.Compare(a.Ref, b.Ref) })
	return union, nil
}

func emitJSONSchema(defs []*def) (string, error) {
	doc := &jsonschema.Schema{Schema: "https://json-schema.org/draft/2020-12/schema", Defs: map[string]*jsonschema.Schema{}}
	for _, d := range defs {
		doc.Defs[d.name] = d.schema
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}

// addFiles merges emitter outputs, which must not share file names.
func addFiles(files, add map[string]string) error {
	for name, content := range add {
		if _, taken := files[name]; taken {
			return fmt.Errorf("two emitters write %s", name)
		}
		files[name] = content
	}
	return nil
}

// writeDir replaces every file with the given extension in dir by files.
func writeDir(dir, ext string, files map[string]string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	existing, err := filepath.Glob(filepath.Join(dir, "*"+ext))
	if err != nil {
		return err
	}
	var errs []error
	for _, path := range existing {
		if _, keep := files[filepath.Base(path)]; !keep {
			errs = append(errs, os.Remove(path))
		}
	}
	for name, content := range files {
		if !strings.HasSuffix(name, ext) {
			return fmt.Errorf("%s: expected a %s file", name, ext)
		}
		errs = append(errs, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	return errors.Join(errs...)
}
