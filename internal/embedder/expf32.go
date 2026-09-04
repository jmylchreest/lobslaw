package embedder

import "math"

// A float32 exponential, because the float64 one was the hot spot.
//
// softmaxRows called math.Exp once per attention score, converting to
// float64 and back each time. A CPU profile of a live node put
// math.archExp at 19.6% of everything — more, by then, than the SIMD
// dot product it sits beside, because the matmuls had been vectorised
// and this had not.
//
// Go's math.Exp is assembly and good. It is beaten here only by not
// doing the work: the inputs and outputs are float32, the result is
// immediately rounded to float32, and the extra precision is discarded
// before anything reads it.
//
// Accuracy is the constraint, not a nice-to-have. The parity gate in
// golden_test.go passes at 1.5e-6 max-abs-diff per element, measured
// so that the tanh GELU approximation (4.23e-04) fails it and honest
// float32 reassociation (3.19e-07) does not. A sloppy exp lands on the
// wrong side of that and every vector this model produces is quietly a
// little worse forever. This is 1.16e-07 relative at worst across
// [-80, 2], the range softmax actually sees after its max subtraction.

// expf32 returns e**x.
//
// exp(x) = 2^k * exp(r), with k = round(x*log2e) and r = x - k*ln2.
// r lands in [-ln2/2, ln2/2] where a degree-5 minimax polynomial is
// accurate to float32, and 2^k is assembled directly into the exponent
// field rather than computed.
func expf32(x float32) float32 {
	const (
		log2e = 1.44269504088896341
		// ln2 split so that k*ln2hi is exact in float32 and the
		// remainder carries the bits that would otherwise be lost.
		ln2hi = 0.693145751953125
		ln2lo = 1.428606765330187e-06
		// 1.5 * 2^23. Adding it forces the mantissa to drop the
		// fractional bits, so subtracting it again leaves the value
		// rounded to nearest — the same answer math.Round gives,
		// without the call that stopped this being worth doing.
		magic = 12582912.0
	)
	t := x*log2e + magic
	fk := t - magic
	k := int32(fk)

	r := (x - fk*ln2hi) - fk*ln2lo

	p := float32(1.9875691500e-4)
	p = p*r + 1.3981999507e-3
	p = p*r + 8.3334519073e-3
	p = p*r + 4.1665795894e-2
	p = p*r + 1.6666665459e-1
	p = p*r + 5.0000001201e-1
	y := 1 + r + p*r*r

	// Underflow guard. Softmax subtracts the row max before calling
	// this, so x is never positive and a very negative score is the
	// normal case rather than an edge one; without the clamp the
	// exponent field wraps and produces a large number where zero was
	// meant, which is a wrong answer that looks like a plausible one.
	if k < -126 {
		return 0
	}
	if k > 127 {
		return float32(math.Inf(1))
	}
	return y * math.Float32frombits(uint32(127+k)<<23)
}
