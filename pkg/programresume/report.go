// Package programresume implements authenticated OSC 7577 reports.
package programresume

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const TokenEnv = "OSC7577_TOKEN"

func ValidToken(s string) bool {
	if len(s) < 22 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func validText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// Report returns an empty string if the command cannot be safely represented.
func Report(token string, pid int, now time.Time, argv []string, cwd string) string {
	if !ValidToken(token) || pid <= 0 || now.Unix() <= 0 || len(argv) == 0 || len(argv) > 64 || !filepath.IsAbs(argv[0]) || !filepath.IsAbs(cwd) || !validText(cwd) {
		return ""
	}
	encoded := make([]string, len(argv))
	for i, arg := range argv {
		if !validText(arg) {
			return ""
		}
		encoded[i] = base64.StdEncoding.EncodeToString([]byte(arg))
	}
	body := fmt.Sprintf("pid=%d:time=%d:argv=%s:cwd=%s", pid, now.Unix(), strings.Join(encoded, ","), base64.StdEncoding.EncodeToString([]byte(cwd)))
	result := sequence(token, body)
	if len(result) > 2048 {
		return ""
	}
	return result
}

func Clear(token string, pid int, now time.Time) string {
	if !ValidToken(token) || pid <= 0 || now.Unix() <= 0 {
		return ""
	}
	return sequence(token, fmt.Sprintf("pid=%d:time=%d:clear=1", pid, now.Unix()))
}

func sequence(token, body string) string {
	sum := sha256.Sum256([]byte(body + ":" + token))
	return fmt.Sprintf("\x1b]7577;%s:mac=%x\x1b\\", body, sum)
}
