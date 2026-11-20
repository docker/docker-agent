package main

import (
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

// jsonErrors writes what this toolchain's encoding/json says about malformed
// tool arguments. The Rust port reproduces these messages; its tests replay
// this corpus, so a toolchain that words them differently is caught here.
func jsonErrors(w io.Writer) error {
	seen := make(map[string]bool)
	first := true
	add := func(input string) error {
		if seen[input] || !utf8.ValidString(input) {
			return nil
		}
		seen[input] = true
		var message string
		var v any
		if err := json.Unmarshal([]byte(input), &v); err != nil {
			message = err.Error()
		}
		line, err := json.Marshal([2]string{input, message})
		if err != nil {
			return err
		}
		separator := ",\n"
		if first {
			separator, first = "[\n", false
		}
		_, err = fmt.Fprintf(w, "%s%s", separator, line)
		return err
	}
	for _, input := range []string{
		``, ` `, "\t\r\n", `{}`, `[]`, ` {} `, `null`, `nul`, `nulL`, `tru`, `trux`, `fals`, `falsy`, `truex`, `x`, `'`, "`", `é`, "\u2028", "\ufeff{}",
		`-`, `-x`, `--1`, `+1`, `.5`, `0`, `-0`, `01`, `1.`, `1.x`, `1e`, `1e+`, `1ex`, `1e+x`, `1.5e+10`, `1E5`, `0x10`, `1 2`, `1,`,
		`"`, `"a`, `"a\`, `"\x"`, `"\ "`, "\"\\\n\"", "\"\\`\"", `"\é"`, `"\u"`, `"\u1`, `"\u12"`, `"\u12G4"`, `"\u12 4"`, "\"\\u12`4\"", `"\u12é4"`, `"\uD83D"`,
		`"\uD83D\uDE00"`, `"\uDE00x"`, `"\u00"`, `"\u00é`, "\"a\tb\"", "\"a\x00b\"", "\"a\x1fb\"", "\"a\x7fb\"", `"é😀"`, `"a"b`, `"a" "b"`,
		`{`, `{ `, `{"`, `{"a`, `{"a"`, `{"a" `, `{"a":`, `{"a": `, `{"a":1`, `{"a":1 `, `{"a":1,`, `{"a":1, `, `{"a":1,}`, `{"a":1,,}`, `{,}`, `{:1}`, `{1:1}`,
		`{a:1}`, `{'a':1}`, `{"a"}`, `{"a" 1}`, `{"a":}`, `{"a"::1}`, `{"a":1 "b":2}`, `{"a":1;"b":2}`, `{"a":1]`, `{"a":[}`, `{"a":{]}`, `{"a":1}}`, `{"a":1}x`,
		`{"a":1,"a":2}`, `{"a":tru}`, `{"a":nul,}`, `{"a":"b` + "\n" + `c"}`, `{"a":NaN}`, `{"a":é}`, `{"a":1é}`, `{"é":1}`, `{é:1}`, `{"a"é1}`,
		`[`, `[ `, `[1`, `[1 `, `[1,`, `[1, `, `[1,]`, `[,1]`, `[1 2]`, `[1;2]`, `[1}`, `[{]`, `[[]`, `[]]`, `[1,é]`, `[1é]`, `[tru]`, `["a",]`, `[1][`,
		`{"a":[1,{"b":[true,{"c":"d"}]}]}`, `{"a":[1,{"b":[true,{"c":"d"}]}]`, `{"a":[1,{"b":[true,{"c":"d"}]]}`, `{"a":[1,{"b":[true,{"c":}]}]}`,
	} {
		if err := add(input); err != nil {
			return err
		}
	}
	for _, seed := range []string{
		`{"a":"b\n\u00e9","n":[-2.5e+3,true],"o":{"k":null}}`,
		`[" \"\\\/",0.5E-1,{},false]`,
	} {
		for i := range len(seed) + 1 {
			if err := add(seed[:i]); err != nil {
				return err
			}
			for _, c := range []string{"x", `"`, `\`, "}", ",", "\n", "é"} {
				if i < len(seed) {
					if err := add(seed[:i] + c + seed[i+1:]); err != nil {
						return err
					}
				}
			}
		}
	}
	_, err := io.WriteString(w, "\n]\n")
	return err
}
