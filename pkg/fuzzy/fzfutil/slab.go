// Extracted verbatim from github.com/junegunn/fzf v0.74.4 src/util/slab.go
// (MIT, Copyright (c) 2013-2026 Junegunn Choi). See ../README.md.

package fzfutil

// Slab is a pre-allocated scratch space the matcher reuses across calls to
// avoid allocating its score matrices.
type Slab struct {
	I16 []int16
	I32 []int32
}

// MakeSlab allocates a Slab with the given capacities.
func MakeSlab(size16, size32 int) *Slab {
	return &Slab{
		I16: make([]int16, size16),
		I32: make([]int32, size32),
	}
}
