package embedder

// Weight packing: the layout change that makes a real GEMM kernel
// possible.
//
// THE PROBLEM IT SOLVES. A dot-product kernel reduces one row of a
// against one row of b and produces ONE output element, which means a
// horizontal reduction — eight lanes summed to a scalar — per element
// of c. A single 64x768x768 projection needs 49,152 of them, and the
// profile showed the cost: 28.5% of runtime inside the vector loads
// feeding those reductions, at ~2.5% of the machine's arithmetic peak.
//
// A real GEMM never reduces horizontally. It keeps a TILE of
// accumulators live in vector registers across the whole k loop and
// stores them once, which needs eight consecutive OUTPUT COLUMNS to be
// contiguous in memory — layout [k][n], not the [n][k] that
// HuggingFace ships.
//
// THE REASON THIS IS CHEAP. The weights are static. Every projection
// and feed-forward matrix is fixed for the life of the process, so the
// reordering is paid once at load instead of being fought on all 72
// weight matmuls of every single encode. Attention's Q·K and scores·V
// keep the dot-product kernel: their operands are computed per call,
// so packing them would cost more than it saves — and they are only
// 1.4% of the arithmetic.

// mr and nr — the tile shape — are chosen PER BUILD, in the kernel
// files, because the right answer differs by an order of magnitude
// between them. With vectors, 4x16 is eight accumulator registers.
// Without, 4x16 is sixty-four floats that spill to stack and turn
// every FMA into two memory operations: measured, the packed layout at
// 4x16 made the portable path 1.4x SLOWER than the dot kernel it
// replaced, while making the SIMD path 1.6x faster. The packing
// adapts to whatever nr the build declares.

// packedWeight is a [n][k] weight matrix reordered into panels of nr
// columns, each panel stored [k][nr] so the kernel's inner loop reads
// nr contiguous output columns for a given k.
//
// Columns past n in the final panel are zero-padded rather than
// special-cased, so the kernel has no tail branch. The padding
// contributes zero to the accumulators and is dropped on write-back.
type packedWeight struct {
	data   []float32
	k, n   int
	panels int
}

// packWeight reorders bt[n][k] into panel-major layout.
func packWeight(bt []float32, k, n int) *packedWeight {
	panels := (n + nr - 1) / nr
	w := &packedWeight{
		data:   make([]float32, panels*k*nr),
		k:      k,
		n:      n,
		panels: panels,
	}
	for p := range panels {
		base := p * k * nr
		for jj := range nr {
			col := p*nr + jj
			if col >= n {
				continue // stays zero
			}
			src := bt[col*k : (col+1)*k]
			for x := range k {
				w.data[base+x*nr+jj] = src[x]
			}
		}
	}
	return w
}

// rows returns n, the number of output columns this weight produces.
func (w *packedWeight) rows() int { return w.n }
