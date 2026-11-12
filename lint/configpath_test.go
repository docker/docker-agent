package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestVersionFromImport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		importPath string
		want       int
		ok         bool
	}{
		{"version zero", "github.com/docker/docker-agent/pkg/config/v0", 0, true},
		{"numbered version", "github.com/docker/docker-agent/pkg/config/v15", 15, true},
		{"bare path", "pkg/config/v2", 2, true},
		{"last marker", "example.com/pkg/config/v1/pkg/config/v2", 2, true},
		{"last marker is not versioned", "example.com/pkg/config/v1/pkg/config/latest", 0, false},
		{"missing marker", "example.com/config/v2", 0, false},
		{"empty", "", 0, false},
		{"empty suffix", "pkg/config/", 0, false},
		{"latest", "pkg/config/latest", 0, false},
		{"missing version number", "pkg/config/v", 0, false},
		{"invalid version number", "pkg/config/vx", 0, false},
		{"trailing slash", "pkg/config/v2/", 0, false},
		{"subpackage", "pkg/config/v2/types", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := versionFromImport(tt.importPath)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.ok, ok)
		})
	}
}
