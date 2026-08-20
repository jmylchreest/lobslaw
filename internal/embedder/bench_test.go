package embedder

import (
	"math/rand"
	"os"
	"testing"
)

// Benchmarks exist to answer one question: is the portable path fast
// enough to be the only path on a machine that cannot run the other?
// Run the same benchmark with and without GOEXPERIMENT=simd to see the
// gap on identical hardware.

// The shapes a real forward pass runs, not round numbers.
// multilingual-e5-base at a 32-token sequence: four D->D projections,
// then D->4D and 4D->D for the feed-forward block.
var benchShapes = []struct {
	name    string
	m, k, n int
}{
	{"proj_32x768x768", 32, 768, 768},
	{"ffn_up_32x768x3072", 32, 768, 3072},
	{"ffn_down_32x3072x768", 32, 3072, 768},
	{"attn_scores_32x64x32", 32, 64, 32},
}

func BenchmarkMatmulBT(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	for _, s := range benchShapes {
		a := randFloats(rng, s.m*s.k)
		bt := randFloats(rng, s.n*s.k)
		c := make([]float32, s.m*s.n)
		b.Run(s.name, func(b *testing.B) {
			b.SetBytes(int64(s.m) * int64(s.k) * int64(s.n) * 4)
			b.ReportAllocs()
			for b.Loop() {
				matmulBT(a, bt, c, s.m, s.k, s.n)
			}
		})
	}
}

// The inner kernel alone, isolated from the parallel split, at the
// two widths that dominate: hidden and intermediate.
func BenchmarkDot(b *testing.B) {
	rng := rand.New(rand.NewSource(2))
	for _, n := range []int{768, 3072} {
		x, y := randFloats(rng, n), randFloats(rng, n)
		b.Run("generic_"+itoa(n), func(b *testing.B) {
			b.SetBytes(int64(n) * 4 * 2)
			var sink float32
			for b.Loop() {
				sink = dotGeneric(x, y)
			}
			_ = sink
		})
		b.Run("dispatched_"+itoa(n), func(b *testing.B) {
			b.SetBytes(int64(n) * 4 * 2)
			var sink float32
			for b.Loop() {
				sink = dot(x, y)
			}
			_ = sink
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// End-to-end, which is the number that actually decides whether this
// is viable on the memory write path. Needs the checkpoint, so it
// skips without one.
func BenchmarkEmbed(b *testing.B) {
	dir := os.Getenv("LOBSLAW_EMBEDDER_MODEL")
	if dir == "" {
		b.Skip("set LOBSLAW_EMBEDDER_MODEL to a HF snapshot directory")
	}
	m, err := Load(dir)
	if err != nil {
		b.Fatal(err)
	}
	// A short memory and a paragraph-length one: cost is roughly
	// linear in the projections and quadratic in attention, so one
	// length does not characterise it.
	for _, n := range []int{16, 64, 256} {
		ids := make([]int32, n)
		for i := range ids {
			ids[i] = int32(1000 + i)
		}
		b.Run("tokens_"+itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = m.Embed(ids)
			}
		})
	}
}
