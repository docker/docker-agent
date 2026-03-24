package lean

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	modShift = 1
	modAlt   = 2
	modCtrl  = 4
	lockMask = 64 + 128

	codepointEscape    = 27
	codepointTab       = 9
	codepointEnter     = 13
	codepointSpace     = 32
	codepointBackspace = 127

	codepointArrowUp    = -1
	codepointArrowDown  = -2
	codepointArrowRight = -3
	codepointArrowLeft  = -4

	codepointDelete   = -10
	codepointInsert   = -11
	codepointHome     = -12
	codepointEnd      = -13
	codepointPageUp   = -14
	codepointPageDown = -15
)

var (
	kittyCSIURe     = regexp.MustCompile(`^\x1b\[(\d+)(?::(\d*))?(?::(\d+))?(?:;(\d+))?(?::(\d+))?u$`)
	kittyArrowRe    = regexp.MustCompile(`^\x1b\[1;(\d+)(?::(\d+))?([ABCD])$`)
	kittyFuncRe     = regexp.MustCompile(`^\x1b\[(\d+)(?:;(\d+))?(?::(\d+))?~$`)
	kittyHomeEndRe  = regexp.MustCompile(`^\x1b\[1;(\d+)(?::(\d+))?([HF])$`)
	modifyOtherRe   = regexp.MustCompile(`^\x1b\[27;(\d+);(\d+)~$`)
	symbolKeyLookup = map[rune]struct{}{'`': {}, '-': {}, '=': {}, '[': {}, ']': {}, '\\': {}, ';': {}, '\'': {}, ',': {}, '.': {}, '/': {}, '!': {}, '@': {}, '#': {}, '$': {}, '%': {}, '^': {}, '&': {}, '*': {}, '(': {}, ')': {}, '_': {}, '+': {}, '|': {}, '~': {}, '{': {}, '}': {}, ':': {}, '<': {}, '>': {}, '?': {}}
)

type parsedKey struct {
	codepoint int
	modifier  int
	eventType int
}

func ParseKey(data string) string {
	switch data {
	case keyShiftEnter:
		return "shift+enter"
	case keyAltEnter:
		return "alt+enter"
	}

	if kitty := parseKittySequence(data); kitty != nil {
		if kitty.eventType == 3 {
			return ""
		}
		return formatParsedKey(kitty.codepoint, kitty.modifier)
	}
	if mod := parseModifyOtherKeysSequence(data); mod != nil {
		return formatParsedKey(mod.codepoint, mod.modifier)
	}

	switch data {
	case keyEscape:
		return "escape"
	case "\t":
		return "tab"
	case keyEnter, "\n":
		return "enter"
	case "\x00":
		return "ctrl+space"
	case " ":
		return "space"
	case keyBackspace, "\x08":
		return "backspace"
	case "\x1b[Z":
		return "shift+tab"
	case keyUp:
		return "up"
	case keyDown:
		return "down"
	case keyLeft:
		return "left"
	case keyRight:
		return "right"
	case keyHome, "\x1bOH":
		return "home"
	case keyEnd, "\x1bOF":
		return "end"
	case keyDelete:
		return "delete"
	case "\x1b[5~":
		return "pageUp"
	case "\x1b[6~":
		return "pageDown"
	}

	if len(data) == 1 {
		b := data[0]
		if b >= 1 && b <= 26 {
			return "ctrl+" + string(rune(b+96))
		}
		if b >= 32 && b < 127 {
			return string(b)
		}
	}

	if strings.HasPrefix(data, "\x1b") && len(data) == 2 {
		if b := data[1]; b >= 'a' && b <= 'z' {
			return "alt+" + string(b)
		}
	}

	return ""
}

func DecodePrintable(data string) string {
	if s := decodeKittyPrintable(data); s != "" {
		return s
	}
	if key := ParseKey(data); key != "" {
		switch key {
		case "space":
			return " "
		case "tab":
			return "\t"
		}
		if len(key) == 1 {
			return key
		}
		return ""
	}
	if strings.HasPrefix(data, "\x1b") {
		return ""
	}
	if data == "" {
		return ""
	}
	r, _ := utf8.DecodeRuneInString(data)
	if r == utf8.RuneError || r < 32 {
		return ""
	}
	return data
}

func decodeKittyPrintable(data string) string {
	m := kittyCSIURe.FindStringSubmatch(data)
	if m == nil {
		return ""
	}
	codepoint := atoiDefault(m[1], -1)
	if codepoint < 0 {
		return ""
	}
	shifted := -1
	if m[2] != "" {
		shifted = atoiDefault(m[2], -1)
	}
	modifier := atoiDefault(m[4], 1) - 1
	eventType := atoiDefault(m[5], 1)
	if eventType == 3 {
		return ""
	}
	modifier &^= lockMask
	if modifier&(modAlt|modCtrl) != 0 {
		return ""
	}
	effective := codepoint
	if modifier&modShift != 0 && shifted > 0 {
		effective = shifted
	}
	if effective < 32 {
		return ""
	}
	return string(rune(effective))
}

func parseKittySequence(data string) *parsedKey {
	if m := kittyCSIURe.FindStringSubmatch(data); m != nil {
		return &parsedKey{codepoint: atoiDefault(m[1], -1), modifier: atoiDefault(m[4], 1) - 1, eventType: atoiDefault(m[5], 1)}
	}
	if m := kittyArrowRe.FindStringSubmatch(data); m != nil {
		modifier := atoiDefault(m[1], 1) - 1
		codepoint := codepointArrowUp
		switch m[3] {
		case "B":
			codepoint = codepointArrowDown
		case "C":
			codepoint = codepointArrowRight
		case "D":
			codepoint = codepointArrowLeft
		}
		return &parsedKey{codepoint: codepoint, modifier: modifier, eventType: atoiDefault(m[2], 1)}
	}
	if m := kittyFuncRe.FindStringSubmatch(data); m != nil {
		modifier := atoiDefault(m[2], 1) - 1
		eventType := atoiDefault(m[3], 1)
		funcCodes := map[int]int{2: codepointInsert, 3: codepointDelete, 5: codepointPageUp, 6: codepointPageDown, 7: codepointHome, 8: codepointEnd}
		if cp, ok := funcCodes[atoiDefault(m[1], -1)]; ok {
			return &parsedKey{codepoint: cp, modifier: modifier, eventType: eventType}
		}
	}
	if m := kittyHomeEndRe.FindStringSubmatch(data); m != nil {
		modifier := atoiDefault(m[1], 1) - 1
		codepoint := codepointHome
		if m[3] == "F" {
			codepoint = codepointEnd
		}
		return &parsedKey{codepoint: codepoint, modifier: modifier, eventType: atoiDefault(m[2], 1)}
	}
	return nil
}

func parseModifyOtherKeysSequence(data string) *parsedKey {
	m := modifyOtherRe.FindStringSubmatch(data)
	if m == nil {
		return nil
	}
	return &parsedKey{codepoint: atoiDefault(m[2], -1), modifier: atoiDefault(m[1], 1) - 1, eventType: 1}
}

func formatParsedKey(codepoint, modifier int) string {
	modifier &^= lockMask
	keyName := ""
	switch codepoint {
	case codepointEscape:
		keyName = "escape"
	case codepointTab:
		keyName = "tab"
	case codepointEnter:
		keyName = "enter"
	case codepointSpace:
		keyName = "space"
	case codepointBackspace:
		keyName = "backspace"
	case codepointDelete:
		keyName = "delete"
	case codepointInsert:
		keyName = "insert"
	case codepointHome:
		keyName = "home"
	case codepointEnd:
		keyName = "end"
	case codepointPageUp:
		keyName = "pageUp"
	case codepointPageDown:
		keyName = "pageDown"
	case codepointArrowUp:
		keyName = "up"
	case codepointArrowDown:
		keyName = "down"
	case codepointArrowLeft:
		keyName = "left"
	case codepointArrowRight:
		keyName = "right"
	default:
		r := rune(codepoint)
		if r >= '0' && r <= '9' {
			keyName = string(r)
		} else if r >= 'a' && r <= 'z' {
			keyName = string(r)
		} else if _, ok := symbolKeyLookup[r]; ok {
			keyName = string(r)
		}
	}
	if keyName == "" {
		return ""
	}
	return formatKeyNameWithModifiers(keyName, modifier)
}

func formatKeyNameWithModifiers(keyName string, modifier int) string {
	modifier &^= lockMask
	parts := make([]string, 0, 4)
	if modifier&modCtrl != 0 {
		parts = append(parts, "ctrl")
	}
	if modifier&modShift != 0 {
		parts = append(parts, "shift")
	}
	if modifier&modAlt != 0 {
		parts = append(parts, "alt")
	}
	parts = append(parts, keyName)
	return strings.Join(parts, "+")
}
