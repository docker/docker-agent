package hcl

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// Source mirrors config.Source so this package can decorate agent sources
// without importing pkg/config.
type Source interface {
	Name() string
	ParentDir() string
	Read(ctx context.Context) ([]byte, error)
}

// NewSource decorates an agent source so HCL documents are transparently
// converted to YAML on read. Detection uses the source name's .hcl extension
// or, when that is absent (OCI artifacts, URLs), the content itself; anything
// else passes through untouched. config.Load only understands YAML, so wrap
// sources that may carry HCL before loading them.
func NewSource(inner Source) Source {
	return source{inner}
}

// IsHCLSource reports whether a source named name holding data should be
// parsed as HCL: the .hcl extension wins, otherwise the content is sniffed.
func IsHCLSource(name string, data []byte) bool {
	return strings.EqualFold(filepath.Ext(name), ".hcl") || LooksLikeHCL(data)
}

type source struct{ Source }

func (s source) Read(ctx context.Context) ([]byte, error) {
	data, err := s.Source.Read(ctx)
	if err != nil {
		return nil, err
	}
	if !IsHCLSource(s.Name(), data) {
		return data, nil
	}
	converted, err := ToYAML(data, s.Name())
	if err != nil {
		return nil, fmt.Errorf("parsing HCL config file: %w", err)
	}
	return converted, nil
}

// encryptedConfigSource mirrors config.EncryptedConfigSource so this package
// can forward the capability without importing pkg/config.
type encryptedConfigSource interface {
	EncryptedConfig() string
}

// EncryptedConfig forwards the inner source's captured encrypted agent config
// when it implements the capability, so the HCL decorator stays transparent to
// callers that type-assert for it (e.g. teamloader wiring the value to the
// Docker models gateway). Returns "" when the inner source does not support it.
func (s source) EncryptedConfig() string {
	if ecs, ok := s.Source.(encryptedConfigSource); ok {
		return ecs.EncryptedConfig()
	}
	return ""
}
