package hcl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

func fileFunction(baseDir string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{
			Name: "path",
			Type: cty.String,
		}},
		Type: function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			path := args[0].AsString()

			data, readPath, err := readFileForHCL(path, baseDir)
			if err != nil {
				return cty.NilVal, fmt.Errorf("reading file %q: %w", readPath, err)
			}
			return cty.StringVal(string(data)), nil
		},
	})
}

func readFileForHCL(path, baseDir string) ([]byte, string, error) {
	if baseDir == "" {
		data, err := os.ReadFile(path)
		return data, path, err
	}
	if !filepath.IsLocal(path) {
		return nil, path, errors.New("path must be a local relative path inside the config directory")
	}

	root, err := os.OpenRoot(baseDir)
	if err != nil {
		return nil, baseDir, fmt.Errorf("opening config directory %q: %w", baseDir, err)
	}
	defer root.Close()

	data, err := root.ReadFile(filepath.ToSlash(path))
	return data, filepath.Join(baseDir, path), err
}
