package main

import (
	"path/filepath"
	"regexp"

	"github.com/dgageot/rubocop-go/cop"
)

var frozenConfigPath = regexp.MustCompile(`(^|/)pkg/config/v\d+/`)

// Shipped config versions keep their original wire format and implementation.
func outsideFrozenConfig(p *cop.Pass) bool {
	return !frozenConfigPath.MatchString(filepath.ToSlash(p.Filename()))
}
