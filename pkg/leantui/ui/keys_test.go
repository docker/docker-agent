package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func singleKey(t *testing.T, b string) Key {
	t.Helper()
	p := &InputParser{}
	keys := p.Feed([]byte(b))
	require.Len(t, keys, 1)
	return keys[0]
}

func TestParseSimplekeys(t *testing.T) {
	t.Parallel()
	assert.Equal(t, KeyEnter, singleKey(t, "\r").Typ)
	assert.Equal(t, KeyEnter, singleKey(t, "\n").Typ)
	assert.Equal(t, KeyTab, singleKey(t, "\t").Typ)
	assert.Equal(t, KeyBackspace, singleKey(t, "\x7f").Typ)
	assert.Equal(t, KeyBackspace, singleKey(t, "\x08").Typ)
	assert.Equal(t, KeyCtrlC, singleKey(t, "\x03").Typ)
	assert.Equal(t, KeyCtrlD, singleKey(t, "\x04").Typ)
	assert.Equal(t, KeyHome, singleKey(t, "\x01").Typ)
	assert.Equal(t, KeyEnd, singleKey(t, "\x05").Typ)
	assert.Equal(t, KeyCtrlW, singleKey(t, "\x17").Typ)
}

func TestParseRunes(t *testing.T) {
	t.Parallel()
	k := singleKey(t, "a")
	assert.Equal(t, KeyRune, k.Typ)
	assert.Equal(t, []rune{'a'}, k.Runes)

	k = singleKey(t, "é")
	assert.Equal(t, KeyRune, k.Typ)
	assert.Equal(t, []rune{'é'}, k.Runes)
}

func TestParseEscapeSequences(t *testing.T) {
	t.Parallel()
	assert.Equal(t, KeyUp, singleKey(t, "\x1b[A").Typ)
	assert.Equal(t, KeyDown, singleKey(t, "\x1b[B").Typ)
	assert.Equal(t, KeyRight, singleKey(t, "\x1b[C").Typ)
	assert.Equal(t, KeyLeft, singleKey(t, "\x1b[D").Typ)
	assert.Equal(t, KeyUp, singleKey(t, "\x1bOA").Typ)
	assert.Equal(t, KeyWordRight, singleKey(t, "\x1b[1;5C").Typ)
	assert.Equal(t, KeyWordLeft, singleKey(t, "\x1b[1;5D").Typ)
	assert.Equal(t, KeyDelete, singleKey(t, "\x1b[3~").Typ)
	assert.Equal(t, KeyHome, singleKey(t, "\x1b[H").Typ)
	assert.Equal(t, KeyEnd, singleKey(t, "\x1b[F").Typ)
	assert.Equal(t, KeyShiftTab, singleKey(t, "\x1b[Z").Typ)
	assert.Equal(t, KeyWordLeft, singleKey(t, "\x1bb").Typ)
	assert.Equal(t, KeyWordRight, singleKey(t, "\x1bf").Typ)
	assert.Equal(t, KeyAltEnter, singleKey(t, "\x1b\r").Typ)
}

func TestParseKittyKeyboardSequences(t *testing.T) {
	t.Parallel()
	tests := []struct {
		sequence string
		want     KeyType
	}{
		{sequence: "\x1b[9;2u", want: KeyShiftTab},
		{sequence: "\x1b[13;2u", want: KeyShiftEnter},
		{sequence: "\x1b[13;3u", want: KeyAltEnter},
		{sequence: "\x1b[27u", want: KeyEsc},
		{sequence: "\x1b[27;1u", want: KeyEsc},
		{sequence: "\x1b[127;3u", want: KeyCtrlW},
		{sequence: "\x1b[8;3u", want: KeyCtrlW},
		{sequence: "\x1b[97;5u", want: KeyHome},
		{sequence: "\x1b[65;5u", want: KeyHome},
		{sequence: "\x1b[99;5u", want: KeyCtrlC},
		{sequence: "\x1b[100;5u", want: KeyCtrlD},
		{sequence: "\x1b[101;5u", want: KeyEnd},
		{sequence: "\x1b[69;5u", want: KeyEnd},
		{sequence: "\x1b[117;5u", want: KeyCtrlU},
		{sequence: "\x1b[107;5u", want: KeyCtrlK},
		{sequence: "\x1b[119;5u", want: KeyCtrlW},
		{sequence: "\x1b[108;5u", want: KeyCtrlL},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, singleKey(t, tt.sequence).Typ)
	}
}

func TestParseLoneEscape(t *testing.T) {
	t.Parallel()
	assert.Equal(t, KeyEsc, singleKey(t, "\x1b").Typ)
}

func TestParseBracketedPaste(t *testing.T) {
	t.Parallel()
	k := singleKey(t, "\x1b[200~hello world\x1b[201~")
	assert.Equal(t, KeyPaste, k.Typ)
	assert.Equal(t, "hello world", string(k.Runes))
}

func TestParseBracketedPasteAcrossReads(t *testing.T) {
	t.Parallel()
	p := &InputParser{}
	assert.Empty(t, p.Feed([]byte("\x1b[200~hel")))
	assert.Empty(t, p.Feed([]byte("lo")))
	keys := p.Feed([]byte(" there\x1b[201~"))
	require.Len(t, keys, 1)
	assert.Equal(t, KeyPaste, keys[0].Typ)
	assert.Equal(t, "hello there", string(keys[0].Runes))
}

func TestParseMixedRun(t *testing.T) {
	t.Parallel()
	p := &InputParser{}
	keys := p.Feed([]byte("hi\r"))
	require.Len(t, keys, 3)
	assert.Equal(t, KeyRune, keys[0].Typ)
	assert.Equal(t, KeyRune, keys[1].Typ)
	assert.Equal(t, KeyEnter, keys[2].Typ)
}
