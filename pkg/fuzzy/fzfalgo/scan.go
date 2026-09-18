// Extracted from github.com/junegunn/fzf v0.74.4 src/algo/indexbyte2_other.go
// and src/algo/runeindex_ref.go (MIT, Copyright (c) 2013-2026 Junegunn Choi).
// See ../README.md.
//
// Upstream ships assembly (amd64, arm64) and byte-view (386) variants of these
// scanners behind build tags. Only the portable reference implementations are
// kept here, so they are the implementation on every platform.

package fzfalgo

import (
	"bytes"
	"slices"
)

// indexByteTwo returns the index of the first occurrence of b1 or b2 in s,
// or -1 if neither is present.
func indexByteTwo(s []byte, b1, b2 byte) int {
	i1 := bytes.IndexByte(s, b1)
	if i1 == 0 {
		return 0
	}
	scope := s
	if i1 > 0 {
		scope = s[:i1]
	}
	if i2 := bytes.IndexByte(scope, b2); i2 >= 0 {
		return i2
	}
	return i1
}

// lastIndexByteTwo returns the index of the last occurrence of b1 or b2 in s,
// or -1 if neither is present.
func lastIndexByteTwo(s []byte, b1, b2 byte) int {
	for i, c := range slices.Backward(s) {
		if c == b1 || c == b2 {
			return i
		}
	}
	return -1
}

func indexASCIIRune(runes []rune, caseSensitive bool, b byte, from int) int {
	lower, upper := rune(b), rune(-1)
	if !caseSensitive && b >= 'a' && b <= 'z' {
		upper = rune(b - 32)
	}
	for i := from; i < len(runes); i++ {
		if runes[i] == lower || runes[i] == upper {
			return i
		}
	}
	return -1
}

func lastIndexASCIIRune(runes []rune, caseSensitive bool, b byte, from int) int {
	lower, upper := rune(b), rune(-1)
	if !caseSensitive && b >= 'a' && b <= 'z' {
		upper = rune(b - 32)
	}
	for i := len(runes) - 1; i >= from; i-- {
		if runes[i] == lower || runes[i] == upper {
			return i
		}
	}
	return -1
}

func indexRune(runes []rune, r rune, from int) int {
	for i := from; i < len(runes); i++ {
		if runes[i] == r {
			return i
		}
	}
	return -1
}

func lastIndexRune(runes []rune, r rune, from int) int {
	for i := len(runes) - 1; i >= from; i-- {
		if runes[i] == r {
			return i
		}
	}
	return -1
}
