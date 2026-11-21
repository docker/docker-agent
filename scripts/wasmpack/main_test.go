package main

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBase = 0x1000

// findfunctab builds the table the linker emits for functions wasm
// functions: sixteen buckets per function, and one last truncated bucket.
func findfunctab(functions int) []byte {
	var tab []byte
	for index := range functions - 1 {
		tab = append(tab, group(index)...)
	}
	tab = binary.LittleEndian.AppendUint32(tab, uint32(functions-1))
	return append(tab, 0)
}

// goImage is a data image shaped like the linker's: read-only data, the
// findfunctab, then data with runs of zeroes.
func goImage(functions int) []byte {
	image := bytes.Repeat([]byte("rodata.."), 64)
	image = append(image, findfunctab(functions)...)
	for i := range 64 {
		image = append(image, bytes.Repeat([]byte{byte(i + 1)}, 24)...)
		image = append(image, make([]byte, i)...)
	}
	return image
}

// module assembles a module with one imported and one defined function, an
// exported 16-page memory and the given data section, like a Go js/wasm
// binary. A changes section replaces the one with its id, drops it when it
// has no body, or is added before the code.
func module(data []byte, changes ...section) []byte {
	sections := []section{
		{secType, []byte{0x01, 0x60, 0x00, 0x00}},
		{secImport, append([]byte{0x01, 0x04}, "gojs\x05debug\x00\x00"...)},
		{secFunction, []byte{0x01, 0x00}},
		{secMemory, []byte{0x01, 0x00, 0x10}},
		{7, append([]byte{0x01, 0x03}, "mem\x02\x00"...)}, // export "mem"
		{0, append([]byte{0x04}, "note"...)},              // custom section
		{secCode, []byte{0x01, 0x02, 0x00, 0x0b}},
		{secData, data},
	}
	for _, change := range changes {
		i := slices.IndexFunc(sections, func(s section) bool { return s.id == change.id })
		switch {
		case i < 0:
			sections = slices.Insert(sections, len(sections)-2, change)
		case change.body == nil:
			sections = slices.Delete(sections, i, i+1)
		default:
			sections[i] = change
		}
	}

	var out bytes.Buffer
	out.WriteString("\x00asm\x01\x00\x00\x00")
	for _, s := range sections {
		writeSection(&out, s.id, s.body)
	}
	return out.Bytes()
}

func sectionsByID(t *testing.T, module []byte) map[byte][]byte {
	t.Helper()
	sections, err := parseSections(module)
	require.NoError(t, err)
	byID := map[byte][]byte{}
	for _, s := range sections {
		byID[s.id] = s.body
	}
	return byID
}

func TestPackRebuildsTheTableAtStart(t *testing.T) {
	const functions = 1000
	image := goImage(functions)
	original := module(encodeData([]segment{{addr: testBase, data: image}}))

	packed, stats, err := pack(original)
	require.NoError(t, err)
	assert.Equal(t, functions, stats.functions)
	assert.Less(t, len(packed), len(original)-functions*groupSize*9/10, "the table is gone from the file")

	sections := sectionsByID(t, packed)
	assert.Equal(t, []byte{0x02}, sections[secStart], "the start function follows the import and the defined function")
	assert.Equal(t, []byte{0x02, 0x00, 0x01}, sections[secFunction])
	assert.Equal(t, []byte{0x02, 0x60, 0x00, 0x00, 0x60, 0x00, 0x00}, sections[secType])

	// The data section holds everything but the table...
	segments, err := parseData(sections[secData])
	require.NoError(t, err)
	base, remaining := memoryImage(segments)
	tab, ok := findTable(image)
	require.True(t, ok)
	want := bytes.Clone(image)
	clear(want[tab.off : tab.off+tab.size()])
	assert.Equal(t, bytes.TrimRight(want, "\x00"), remaining)
	assert.Equal(t, testBase, base)
	// ...and the start function is given exactly what was removed.
	assert.Equal(t, image[tab.off:tab.off+tab.size()], tab.contents())
	assert.Equal(t, findfunctab(functions)[groupSize:groupSize+tab.size()], tab.contents(), "all but the zeroed group of function 0 and the sub-bucket byte")

	for _, s := range segments {
		assert.NotZero(t, s.data[0])
		assert.NotZero(t, s.data[len(s.data)-1])
		assert.NotContains(t, string(s.data), string(make([]byte, segmentOverhead)), "long runs of zeroes are left out")
	}

	// A second pass finds nothing left to remove.
	_, _, err = pack(packed)
	require.ErrorContains(t, err, "already has a start function")
}

// The real proof: both modules leave the same memory once instantiated.
func TestPackedModuleInstantiatesToTheSameMemory(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}

	for name, original := range map[string][]byte{
		"one segment":    module(encodeData([]segment{{addr: testBase, data: goImage(1000)}})),
		"split segments": module(encodeData(split(testBase, goImage(700)))),
		"data count":     module(encodeData([]segment{{addr: testBase, data: goImage(300)}}), section{id: secDataCount, body: []byte{0x01}}),
	} {
		t.Run(name, func(t *testing.T) {
			packed, _, err := pack(original)
			require.NoError(t, err)

			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "original.wasm"), original, 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "packed.wasm"), packed, 0o600))
			const script = `
const fs = require("node:fs");
const memory = (file) => {
  const module = new WebAssembly.Module(fs.readFileSync(file));
  return Buffer.from(new WebAssembly.Instance(module, { gojs: { debug() {} } }).exports.mem.buffer);
};
process.exit(memory("original.wasm").equals(memory("packed.wasm")) ? 0 : 1);`
			cmd := exec.CommandContext(t.Context(), node, "-e", script)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", out)
		})
	}
}

func TestPackLeavesOtherModulesAlone(t *testing.T) {
	data := func(image []byte) []byte { return encodeData([]segment{{addr: testBase, data: image}}) }

	for name, tc := range map[string]struct {
		module []byte
		want   string
	}{
		"no table":            {module(data(bytes.Repeat([]byte("data"), 4096))), errNothingToPack.Error()},
		"no data":             {module(encodeData(nil)), errNothingToPack.Error()},
		"too few functions":   {module(data(goImage(minGroups))), errNothingToPack.Error()},
		"start function":      {module(data(goImage(1000)), section{id: secStart, body: []byte{0x01}}), "already has a start function"},
		"passive segment":     {module(append([]byte{0x01, 0x01, 0x04}, "data"...)), "unsupported data segment kind 1"},
		"computed address":    {module([]byte{0x01, 0x00, 0x23, 0x00, 0x0b, 0x00}), "unsupported data segment offset"},
		"imported memory":     {module(data(goImage(1000)), section{id: secMemory}, section{id: secImport, body: append([]byte{0x01, 0x04}, "gojs\x03mem\x02\x00\x10"...)}), "its own memory"},
		"two memories":        {module(data(goImage(1000)), section{id: secMemory, body: []byte{0x02, 0x00, 0x10, 0x00, 0x10}}), "defines 2 memories"},
		"data beyond memory":  {module(data(goImage(1000)), section{id: secMemory, body: []byte{0x01, 0x00, 0x01}}), "outside the initial memory"},
		"no code":             {module(data(goImage(1000)), section{id: secCode}), "does not define functions"},
		"malformed types":     {module(data(goImage(1000)), section{id: secType, body: []byte{0x80}}), errMalformed.Error()},
		"truncated":           {module(data(goImage(1000)))[:4096], errMalformed.Error()},
		"not a wasm module":   {[]byte("#!/bin/sh"), "not a WebAssembly module"},
		"unknown import kind": {append([]byte("\x00asm\x01\x00\x00\x00\x02\x05\x01\x00\x00\x09\x00"), 0x0b, 0x01, 0x00), "unsupported import kind"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := pack(tc.module)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestSplitStaysWithinTheSegmentLimit(t *testing.T) {
	// More short gaps than there may be segments, and a few long ones.
	var image []byte
	for i := range maxSegments + 5000 {
		image = append(image, 0xff)
		image = append(image, make([]byte, segmentOverhead+i%3)...)
		if i%10000 == 0 {
			image = append(image, make([]byte, 4096)...)
		}
	}
	image = append(image, 0xff)

	segments := split(testBase, image)
	assert.Less(t, len(segments), maxSegments)
	assert.Greater(t, len(segments), maxSegments/2, "only the shortest gaps are given up")
	base, rebuilt := memoryImage(segments)
	assert.Equal(t, testBase, base)
	assert.Equal(t, image, rebuilt)
}

func TestSplitOfAnEmptyImage(t *testing.T) {
	assert.Empty(t, split(testBase, nil))
	assert.Empty(t, split(testBase, make([]byte, 64)))
}

func TestLEB128(t *testing.T) {
	for _, v := range []int64{0, 1, -1, 63, 64, -64, -65, 320, 0x7fffffff, -0x80000000, int64(int32(-0x10000))} {
		r := reader{b: sleb(v)}
		assert.Equal(t, v, r.sleb(), "%d", v)
		assert.Empty(t, r.b)
		require.NoError(t, r.err)
	}
	for _, malformed := range [][]byte{{0x80, 0x80}, uleb(math.MaxUint32 + 1)} {
		r := reader{b: malformed}
		r.u32()
		require.ErrorIs(t, r.err, errMalformed)
	}

	for _, addr := range []int{0, 1, 0x1000, 0x7fffffff, 0x80000000, 0xfffffff0} {
		r := reader{b: i32Const(addr)[1:]}
		assert.Equal(t, addr, r.i32(), "%#x", addr)
	}
}
