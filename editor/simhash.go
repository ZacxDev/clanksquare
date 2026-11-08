package editor

import (
	"hash/fnv"
	"math/bits"
	"strings"
	"unicode"
)

// NearDupBits is the largest Hamming distance between two SimHashes that
// counts as a near-duplicate. 3 of 64 bits is the usual setting for
// web-page near-duplicate detection (Manku et al., WWW 2007).
const NearDupBits = 3

// tokens lowercases s and splits it into runs of letters and digits.
func tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// SimHash is a 64-bit SimHash of s over its word unigrams and bigrams, each
// hashed with FNV-1a and weighted 1. An empty text hashes to 0.
func SimHash(s string) uint64 {
	toks := tokens(s)
	var v [64]int
	add := func(f string) {
		h := fnv.New64a()
		h.Write([]byte(f))
		x := h.Sum64()
		for i := 0; i < 64; i++ {
			if x&(1<<uint(i)) != 0 {
				v[i]++
			} else {
				v[i]--
			}
		}
	}
	for i, t := range toks {
		add(t)
		if i > 0 {
			add(toks[i-1] + " " + t)
		}
	}
	if len(toks) == 0 {
		return 0
	}
	var out uint64
	for i := 0; i < 64; i++ {
		if v[i] > 0 {
			out |= 1 << uint(i)
		}
	}
	return out
}

// Hamming is the number of differing bits.
func Hamming(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// NearDup reports whether two texts are near-duplicates. Two empty texts are
// not: an empty title and excerpt carries no evidence of anything.
func NearDup(a, b string) bool {
	ha, hb := SimHash(a), SimHash(b)
	if len(tokens(a)) == 0 || len(tokens(b)) == 0 {
		return false
	}
	return Hamming(ha, hb) <= NearDupBits
}
