//go:build goexperiment.simd && amd64

package embedder

// gemmTile computes an mr x nr tile of c, portably.
//
// The accumulators are a stack array the compiler keeps in registers
// across the k loop, so there is no horizontal reduction and no store
// to c until the tile is finished — the same structural win the SIMD
// version gets, minus the vector width.
func gemmTileGeneric(a, wp, c []float32, i, p, k, n int) {
	var acc [mr][nr]float32
	for x := range k {
		row := wp[x*nr : x*nr+nr]
		for r := range mr {
			av := a[(i+r)*k+x]
			for jj := range nr {
				acc[r][jj] += av * row[jj]
			}
		}
	}
	for r := range mr {
		base := (i+r)*n + p*nr
		for jj := range nr {
			if p*nr+jj >= n {
				break
			}
			c[base+jj] = acc[r][jj]
		}
	}
}
