package socket

import (
	"strings"
)

func stripUnixScheme(path string) string {
	return strings.TrimPrefix(path, "unix://")
}
