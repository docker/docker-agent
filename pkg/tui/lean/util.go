package lean

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	ansi "github.com/charmbracelet/x/ansi"
)

func wrapPlain(text string, width int, preserveSpace bool) []string {
	if width <= 0 {
		return []string{""}
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	parts := strings.Split(text, "\n")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			out = append(out, "")
			continue
		}
		wrapped := ansi.Wrap(part, width, " ")
		if preserveSpace {
			wrapped = ansi.Hardwrap(part, width, true)
		}
		out = append(out, strings.Split(wrapped, "\n")...)
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

func padLine(text string, width int, style func(...string) string) string {
	if width <= 0 {
		return ""
	}
	truncated := ansi.Truncate(text, width, "")
	padding := max(0, width-ansi.StringWidth(truncated))
	return style(truncated + strings.Repeat(" ", padding))
}

func homeTilde(path string) string {
	if path == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		if path == home {
			return "~"
		}
		if rel, err := filepath.Rel(home, path); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return path
}

func middleTruncate(text string, width int) string {
	if ansi.StringWidth(text) <= width {
		return text
	}
	if width <= 3 {
		return ansi.Truncate(text, width, "")
	}
	runes := []rune(ansi.Strip(text))
	half := (width - 1) / 2
	head := string(runes[:minInt(len(runes), half)])
	tailLen := min(len(runes), maxInt(1, width-ansi.StringWidth(head)-1))
	tail := string(runes[len(runes)-tailLen:])
	return head + "…" + tail
}

func visibleWidth(s string) int { return ansi.StringWidth(s) }

func formatMoney(cost float64) string {
	if cost < 0.01 {
		return fmt.Sprintf("%.4f", cost)
	}
	return fmt.Sprintf("%.2f", cost)
}

func formatTokens(count int64) string {
	if count >= 1_000_000 {
		return fmt.Sprintf("%.1fm", float64(count)/1_000_000)
	}
	if count >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(count)/1_000)
	}
	return strconv.FormatInt(count, 10)
}

func atoiDefault(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return v
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func ternaryInt(cond bool, a, b int) int {
	if cond {
		return a
	}
	return b
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
