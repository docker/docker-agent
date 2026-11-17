// Command wasmpack shrinks a Go js/wasm binary after linking.
//
// On wasm the linker gives every function 64 KiB of PC space, so
// runtime.findfunctab holds 320 bytes per function: sixteen buckets, each
// the function's index followed by sixteen zero bytes. wasmpack removes that
// table from the data section and adds a start function that writes it back
// at instantiation. It then re-splits the remaining data around runs of
// zeroes, which the linker stops doing once the table has used up its
// segment budget.
//
// The table is found by its content and only removed when the start function
// reproduces it byte for byte, so memory after instantiation is unchanged.
// A module without such a table is left as it is.
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"slices"
)

func main() {
	out := flag.String("o", "", "output file (default: rewrite the input)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: wasmpack [-o out.wasm] in.wasm")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(flag.Arg(0), *out); err != nil {
		fmt.Fprintln(os.Stderr, "wasmpack:", err)
		os.Exit(1)
	}
}

func run(in, out string) error {
	module, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	if out == "" {
		out = in
	}

	info, err := os.Stat(in)
	if err != nil {
		return err
	}

	packed, stats, err := pack(module)
	switch {
	case errors.Is(err, errNothingToPack):
		fmt.Fprintf(os.Stderr, "wasmpack: %s: %v, left unchanged\n", in, err)
		if out == in {
			return nil
		}
		packed = module
	case err != nil:
		return fmt.Errorf("%s: %w", in, err)
	default:
		fmt.Printf("wasmpack: %s: %.2f MiB -> %.2f MiB (table of %d functions rebuilt at start, %d -> %d data segments)\n",
			in, mib(len(module)), mib(len(packed)), stats.functions, stats.segmentsBefore, stats.segmentsAfter)
	}
	return os.WriteFile(out, packed, info.Mode().Perm())
}

func mib(n int) float64 { return float64(n) / (1 << 20) }

// errNothingToPack reports a module that has no findfunctab worth removing.
var errNothingToPack = errors.New("no findfunctab found")

const (
	// A findfunc bucket is a uint32 function index and 16 sub-bucket bytes.
	bucketSize = 4 + 16
	// A bucket covers 4 KiB of PC space, and a wasm function 64 KiB.
	bucketsPerFunc = 16
	groupSize      = bucketSize * bucketsPerFunc

	// minGroups keeps short look-alike runs in ordinary data out of it.
	minGroups = 256

	// The linker's limits: a segment costs about 8 bytes, and some runtimes
	// reject modules with more than 100000 of them.
	segmentOverhead = 8
	maxSegments     = 100000
)

const (
	secType      = 1
	secImport    = 2
	secFunction  = 3
	secMemory    = 5
	secStart     = 8
	secElement   = 9
	secCode      = 10
	secData      = 11
	secDataCount = 12
)

type packStats struct {
	functions      int
	segmentsBefore int
	segmentsAfter  int
}

type section struct {
	id   byte
	body []byte
}

type segment struct {
	addr int
	data []byte
}

// table locates a findfunctab in the memory image: groups full groups whose
// indexes count up from 1, then optionally the index of one last function.
type table struct {
	off    int
	groups int
	tail   bool
}

func (t table) size() int {
	size := t.groups * groupSize
	if t.tail {
		size += 4
	}
	return size
}

// functions counts function 0, whose group is all zeroes, the groups and
// the last function.
func (t table) functions() int {
	if t.tail {
		return t.groups + 2
	}
	return t.groups + 1
}

// pack returns module with its findfunctab rebuilt by a start function.
func pack(module []byte) ([]byte, packStats, error) {
	sections, err := parseSections(module)
	if err != nil {
		return nil, packStats{}, err
	}

	var segments []segment
	var imports, functions, memory int
	seen := map[byte]bool{}
	for _, s := range sections {
		seen[s.id] = true
		switch s.id {
		case secStart:
			return nil, packStats{}, errors.New("module already has a start function")
		case secImport:
			imports, err = countFuncImports(s.body)
		case secFunction:
			r := reader{b: s.body}
			functions, err = r.u32(), r.err
		case secMemory:
			memory, err = memorySize(s.body)
		case secData:
			segments, err = parseData(s.body)
		}
		if err != nil {
			return nil, packStats{}, err
		}
	}
	// Removed bytes are only rebuilt, or left as zeroes, in a memory that
	// starts out zeroed: the module's own, not an imported one.
	if !seen[secType] || !seen[secFunction] || !seen[secMemory] || !seen[secCode] {
		return nil, packStats{}, errors.New("module does not define functions and its own memory")
	}
	if len(segments) == 0 {
		return nil, packStats{}, errNothingToPack
	}
	for _, s := range segments {
		if s.addr+len(s.data) > memory {
			return nil, packStats{}, errors.New("data segment outside the initial memory")
		}
	}

	base, image := memoryImage(segments)
	t, ok := findTable(image)
	if !ok {
		return nil, packStats{}, errNothingToPack
	}
	// The start function writes the indexes into zeroed memory: the module
	// is only equivalent if that is exactly what is being removed.
	region := image[t.off : t.off+t.size()]
	if !bytes.Equal(region, t.contents()) {
		return nil, packStats{}, errors.New("findfunctab does not match its regenerated contents")
	}
	clear(region)

	resplit := split(base, image)
	typeIndex := 0
	start := startFunction(base+t.off, t)

	var out bytes.Buffer
	out.Write(module[:8])
	startWritten := false
	for _, s := range sections {
		body := s.body
		switch s.id {
		case secType:
			// A duplicate of an existing type is valid and costs 3 bytes.
			body, typeIndex, err = appendToVec(body, []byte{0x60, 0x00, 0x00})
		case secFunction:
			body, _, err = appendToVec(body, uleb(typeIndex))
		case secCode:
			body, _, err = appendToVec(body, append(uleb(len(start)), start...))
		case secDataCount:
			body = uleb(len(resplit))
		case secData:
			body = encodeData(resplit)
		}
		if err != nil {
			return nil, packStats{}, err
		}
		// The start section sits between the exports and the elements.
		if !startWritten && (s.id == secElement || s.id == secDataCount || s.id == secCode || s.id == secData) {
			writeSection(&out, secStart, uleb(imports+functions))
			startWritten = true
		}
		writeSection(&out, s.id, body)
	}

	return out.Bytes(), packStats{functions: t.functions(), segmentsBefore: len(segments), segmentsAfter: len(resplit)}, nil
}

func parseSections(module []byte) ([]section, error) {
	if !bytes.HasPrefix(module, []byte("\x00asm\x01\x00\x00\x00")) {
		return nil, errors.New("not a WebAssembly module")
	}
	r := reader{b: module[8:]}
	var sections []section
	for len(r.b) > 0 && r.err == nil {
		id := r.byte()
		sections = append(sections, section{id: id, body: r.bytes(r.u32())})
	}
	return sections, r.err
}

func countFuncImports(body []byte) (int, error) {
	r := reader{b: body}
	funcs := 0
	for count := r.u32(); count > 0 && r.err == nil; count-- {
		r.bytes(r.u32()) // module
		r.bytes(r.u32()) // name
		switch kind := r.byte(); kind {
		case 0x00: // function
			r.u32()
			funcs++
		case 0x01: // table
			r.byte()
			r.limits()
		case 0x02: // memory
			r.limits()
		case 0x03: // global
			r.bytes(2)
		default:
			return 0, fmt.Errorf("unsupported import kind %#x", kind)
		}
	}
	return funcs, r.err
}

// memorySize returns the initial size in bytes of the module's one memory.
func memorySize(body []byte) (int, error) {
	r := reader{b: body}
	if count := r.u32(); count != 1 && r.err == nil {
		return 0, fmt.Errorf("module defines %d memories", count)
	}
	r.byte() // limits flags
	pages := r.u32()
	return pages << 16, r.err
}

// parseData reads active segments at constant addresses, the only kind the
// Go linker emits.
func parseData(body []byte) ([]segment, error) {
	r := reader{b: body}
	var segments []segment
	for count := r.u32(); count > 0 && r.err == nil; count-- {
		if kind := r.u32(); kind != 0 {
			return nil, fmt.Errorf("unsupported data segment kind %d", kind)
		}
		if op := r.byte(); op != 0x41 {
			return nil, fmt.Errorf("unsupported data segment offset (opcode %#x)", op)
		}
		addr := r.i32()
		if end := r.byte(); end != 0x0b {
			return nil, errors.New("unsupported data segment offset expression")
		}
		segments = append(segments, segment{addr: addr, data: r.bytes(r.u32())})
	}
	return segments, r.err
}

// memoryImage lays the segments out as they are in memory after
// instantiation, starting at the returned address.
func memoryImage(segments []segment) (int, []byte) {
	lo, hi := segments[0].addr, 0
	for _, s := range segments {
		lo = min(lo, s.addr)
		hi = max(hi, s.addr+len(s.data))
	}
	image := make([]byte, hi-lo)
	for _, s := range segments {
		copy(image[s.addr-lo:], s.data)
	}
	return lo, image
}

// findTable returns the longest run of findfunc groups in image. The group
// of function 0 is all zeroes and needs no rebuilding, so the run starts
// with function 1.
func findTable(image []byte) (table, bool) {
	var best table
	needle := append(group(1), group(2)...)
	for off := 0; ; {
		i := bytes.Index(image[off:], needle)
		if i < 0 {
			break
		}
		t := table{off: off + i, groups: 2}
		for end := t.off + t.size(); end+groupSize <= len(image) && bytes.Equal(image[end:end+groupSize], group(t.groups+1)); end += groupSize {
			t.groups++
		}
		if end := t.off + t.size(); end+4 <= len(image) && int(binary.LittleEndian.Uint32(image[end:])) == t.groups+1 {
			t.tail = true
		}
		if t.groups > best.groups {
			best = t
		}
		off = t.off + t.size()
	}
	return best, best.groups >= minGroups
}

// group is the sixteen buckets of one function.
func group(index int) []byte {
	g := make([]byte, groupSize)
	for bucket := range bucketsPerFunc {
		binary.LittleEndian.PutUint32(g[bucket*bucketSize:], uint32(index)) //nolint:gosec // function indexes fit: the table stores them as uint32
	}
	return g
}

// contents is what startFunction writes for t.
func (t table) contents() []byte {
	contents := make([]byte, 0, t.size())
	for index := 1; index <= t.groups; index++ {
		contents = append(contents, group(index)...)
	}
	if t.tail {
		contents = append(contents, group(t.groups + 1)[:4]...)
	}
	return contents
}

// startFunction is the body of a function that writes t at addr:
//
//	for index := 1; index <= groups; index++ {
//		for bucket := range 16 { store32(p + bucket*20, index) }
//		p += 320
//	}
//	store32(p, index) // tail
func startFunction(addr int, t table) []byte {
	const (
		index = 0
		p     = 1
	)
	code := slices.Concat(
		// Two i32 locals: index = 1, p = addr.
		[]byte{0x01, 0x02, 0x7f},
		i32Const(1), []byte{0x21, index},
		i32Const(addr), []byte{0x21, p},
		// block, loop: leave the block when index > groups.
		[]byte{0x02, 0x40, 0x03, 0x40},
		[]byte{0x20, index}, i32Const(t.groups), []byte{0x4b, 0x0d, 0x01},
	)
	store := func(offset int) []byte {
		// local.get p, local.get index, i32.store align=4
		return append([]byte{0x20, p, 0x20, index, 0x36, 0x02}, uleb(offset)...)
	}
	for bucket := range bucketsPerFunc {
		code = append(code, store(bucket*bucketSize)...)
	}
	code = slices.Concat(code,
		// p += 320, index++, next group.
		[]byte{0x20, p}, i32Const(groupSize), []byte{0x6a, 0x21, p},
		[]byte{0x20, index}, i32Const(1), []byte{0x6a, 0x21, index},
		[]byte{0x0c, 0x00},
		// end loop, end block
		[]byte{0x0b, 0x0b},
	)
	if t.tail {
		code = append(code, store(0)...)
	}
	return append(code, 0x0b) // end
}

// i32Const encodes v, an unsigned 32-bit value, as the signed constant that
// wasm reads back as the same 32 bits.
func i32Const(v int) []byte {
	if v >= 1<<31 {
		v -= 1 << 32
	}
	return append([]byte{0x41}, sleb(int64(v))...)
}

// split cuts image into segments that leave out runs of zeroes long enough
// to pay for a segment, as the linker does. Past the segment limit only the
// longest runs are left out.
func split(base int, image []byte) []segment {
	type run struct{ start, end int }
	var gaps []run
	for off := 0; off < len(image); off++ {
		if image[off] != 0 {
			continue
		}
		start := off
		for off < len(image) && image[off] == 0 {
			off++
		}
		if off-start >= segmentOverhead {
			gaps = append(gaps, run{start, off})
		}
	}
	for shortest := segmentOverhead + 1; len(gaps) >= maxSegments; shortest++ {
		gaps = slices.DeleteFunc(gaps, func(g run) bool { return g.end-g.start < shortest })
	}

	var segments []segment
	add := func(start, end int) {
		for start < end && image[start] == 0 {
			start++
		}
		for end > start && image[end-1] == 0 {
			end--
		}
		if start < end {
			segments = append(segments, segment{addr: base + start, data: image[start:end]})
		}
	}
	next := 0
	for _, gap := range gaps {
		add(next, gap.start)
		next = gap.end
	}
	add(next, len(image))
	return segments
}

func encodeData(segments []segment) []byte {
	body := uleb(len(segments))
	for _, s := range segments {
		body = append(body, 0x00) // active segment in memory 0
		body = append(body, i32Const(s.addr)...)
		body = append(body, 0x0b)
		body = append(body, uleb(len(s.data))...)
		body = append(body, s.data...)
	}
	return body
}

// appendToVec adds entry to the vector that makes up a section body and
// returns the new body and the entry's index.
func appendToVec(body, entry []byte) ([]byte, int, error) {
	r := reader{b: body}
	count := r.u32()
	grown := append(uleb(count+1), r.b...)
	return append(grown, entry...), count, r.err
}

func writeSection(out *bytes.Buffer, id byte, body []byte) {
	out.WriteByte(id)
	out.Write(uleb(len(body)))
	out.Write(body)
}

func uleb(v int) []byte {
	return binary.AppendUvarint(nil, uint64(v)) //nolint:gosec // sizes, counts and indexes are never negative
}

func sleb(v int64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			return append(out, b)
		}
		out = append(out, b|0x80)
	}
}

// reader decodes a section body. After the first error every read returns
// zero values, so callers check err once at the end.
type reader struct {
	b   []byte
	err error
}

var errMalformed = errors.New("malformed module")

func (r *reader) bytes(n int) []byte {
	if r.err != nil || n < 0 || n > len(r.b) {
		r.err = errMalformed
		r.b = nil
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *reader) byte() byte {
	b := r.bytes(1)
	if b == nil {
		return 0
	}
	return b[0]
}

// u32 reads the unsigned 32-bit integers of the format: sizes, counts and
// indexes.
func (r *reader) u32() int {
	v, n := binary.Uvarint(r.b)
	if n <= 0 || v > math.MaxUint32 {
		r.err = errMalformed
		r.b = nil
		return 0
	}
	r.b = r.b[n:]
	return int(v)
}

// i32 reads a signed 32-bit constant as the unsigned address it stands for.
func (r *reader) i32() int {
	return int(uint32(r.sleb())) //nolint:gosec // the low 32 bits are the address
}

func (r *reader) sleb() int64 {
	var v int64
	for shift := uint(0); shift < 64; shift += 7 {
		b := r.byte()
		v |= int64(b&0x7f) << shift
		if b&0x80 == 0 {
			if b&0x40 != 0 && shift+7 < 64 {
				v |= -1 << (shift + 7)
			}
			return v
		}
	}
	r.err = errMalformed
	return 0
}

func (r *reader) limits() {
	flags := r.byte()
	r.u32()
	if flags&0x01 != 0 {
		r.u32()
	}
}
