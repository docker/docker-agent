// Extracted from github.com/junegunn/fzf v0.74.4 src/util/chars.go (MIT,
// Copyright (c) 2013-2026 Junegunn Choi). See ../README.md for provenance and
// the list of deviations.

// Package fzfutil holds the text container used by fzfalgo. Compared with
// upstream it keeps the byte and rune views in two fields instead of one
// unsafe-reinterpreted slice, and drops everything the matcher does not use.
package fzfutil

import "unicode/utf8"

// Chars is a line of text, kept as bytes when it is pure ASCII and as runes
// otherwise. The zero value is an empty ASCII text.
type Chars struct {
	bytes   []byte
	runes   []rune
	inBytes bool
	mayFold bool
}

// Rune ranges that case folding or normalization can turn into ASCII, derived
// from algo's normalization table and unicode.ToLower, then merged. They are a
// superset of the exact set. Grouped tightly on purpose: a wider merge would
// include Greek Extended, General Punctuation and the currency and letterlike
// blocks, and every line holding a curly quote or an em dash would then lose
// the prefilter. Cyrillic, Greek, Hebrew, Arabic, Thai, Devanagari, CJK,
// Hangul, kana, emoji, punctuation and box drawing are all outside.
const (
	foldLo = 0x00C0
	foldHi = 0xFF61
)

var foldableRanges = [...][2]rune{
	{0x00C0, 0x01B6}, // Latin-1 Supplement, Latin Extended-A and -B
	{0x01CD, 0x02AE}, // rest of Latin Extended-B and IPA Extensions
	{0x0363, 0x036F}, // combining Latin small letters
	{0x1D00, 0x1D22}, // Phonetic Extensions, small capitals
	{0x1D62, 0x1D65}, // subscript letters
	{0x1E00, 0x1EF9}, // Latin Extended Additional
	{0x2071, 0x2071}, // superscript i
	{0x2095, 0x209C}, // subscript letters
	{0x212A, 0x212B}, // KELVIN SIGN and ANGSTROM SIGN, which fold by case
	{0x2183, 0x2184}, // reversed roman numeral one hundred
	{0x2C62, 0x2C7F}, // Latin Extended-C
	{0xA78D, 0xA78D}, // Latin Extended-D
	{0xA7AA, 0xA7B2}, // more Latin Extended-D
	{0xA7C5, 0xA7C5},
	{0xFF01, 0xFF61}, // fullwidth ASCII forms, and halfwidth ideographic full stop
}

// Walking the ranges costs a serial chain of comparisons per rune, which is
// measurable at ingestion, so precompute a bitmap instead.
var foldableBits = func() (bits [(foldHi-foldLo)/8 + 1]byte) {
	for _, r := range foldableRanges {
		for c := r[0]; c <= r[1]; c++ {
			i := c - foldLo
			bits[i>>3] |= 1 << (i & 7)
		}
	}
	return
}()

// MayFoldToASCII reports whether case folding or normalization could turn r
// into an ASCII character.
func MayFoldToASCII(r rune) bool {
	i := uint32(r - foldLo) //nolint:gosec // wraparound of r < foldLo is intended
	if i > foldHi-foldLo {
		return false
	}
	return foldableBits[i>>3]&(1<<(i&7)) != 0
}

// ToChars converts a byte array into a Chars. Pure ASCII input is kept as is;
// anything else is decoded into runes, one utf8.RuneError per invalid byte.
func ToChars(bytes []byte) Chars {
	if isASCII(bytes) {
		return Chars{bytes: bytes, inBytes: true}
	}

	runes := make([]rune, 0, len(bytes))
	mayFold := false
	for i := 0; i < len(bytes); {
		if b := bytes[i]; b < utf8.RuneSelf {
			runes = append(runes, rune(b))
			i++
			continue
		}
		r, sz := utf8.DecodeRune(bytes[i:])
		i += sz
		mayFold = mayFold || MayFoldToASCII(r)
		runes = append(runes, r)
	}
	return Chars{runes: runes, mayFold: mayFold}
}

func isASCII(bytes []byte) bool {
	for _, b := range bytes {
		if b >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// IsBytes reports whether the text is pure ASCII and kept as bytes.
func (chars *Chars) IsBytes() bool {
	return chars.inBytes
}

// MayFoldToASCII reports whether the text holds a rune that case folding or
// normalization could turn into an ASCII character. When false, an ASCII
// pattern character can only match the identical ASCII rune, which is what
// lets the prefilter scan the rune array directly.
func (chars *Chars) MayFoldToASCII() bool {
	return chars.mayFold
}

// Runes returns the underlying rune slice, or nil if the text is kept as
// bytes. Read only.
func (chars *Chars) Runes() []rune {
	return chars.runes
}

// Bytes returns the underlying byte slice, or nil if the text is kept as
// runes. Read only.
func (chars *Chars) Bytes() []byte {
	return chars.bytes
}

// Get returns the rune at index i.
func (chars *Chars) Get(i int) rune {
	if chars.inBytes {
		return rune(chars.bytes[i])
	}
	return chars.runes[i]
}

// Length returns the number of runes in the text.
func (chars *Chars) Length() int {
	if chars.inBytes {
		return len(chars.bytes)
	}
	return len(chars.runes)
}

// CopyRunes copies len(dest) runes starting at from into dest.
func (chars *Chars) CopyRunes(dest []rune, from int) {
	if !chars.inBytes {
		copy(dest, chars.runes[from:])
		return
	}
	for idx, b := range chars.bytes[from:][:len(dest)] {
		dest[idx] = rune(b)
	}
}
