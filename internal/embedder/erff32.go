package embedder

// A float32 error function, for GELU.
//
// gelu is x * 0.5 * (1 + erf(x/sqrt2)), and it called math.Erf per
// element. In a profile of the embedding benchmark that path was 17.2%
// of everything — math.erf at 11.5% plus the math.Exp it calls
// internally — which put it above the SIMD dot product.
//
// The form is NOT negotiable. golden_test.go exists partly to catch a
// tanh GELU substituted for the erf one: it is a permanent degradation
// that produces no error, scores 0.9999955 on cosine, and fails the
// per-element gate at 4.23e-04 against a 1.5e-6 budget. So this
// computes erf, accurately, in the precision the surrounding arithmetic
// already uses — it does not compute something else that is close.

// erff32 returns erf(z).
//
// Split at |z| = 1, because one approximation cannot be accurate at
// both ends. Below it the Maclaurin series converges quickly and keeps
// relative accuracy where erf is small and GELU is most sensitive to
// it. Above it erf is heading for 1, absolute accuracy is what matters,
// and the Abramowitz-Stegun 7.1.26 form gets there in five terms and
// one exponential — the fast one from expf32.go.
func erff32(z float32) float32 {
	neg := z < 0
	if neg {
		z = -z
	}

	var y float32
	switch {
	case z >= 4:
		// erf(4) is 1 - 1.5e-8; float32 cannot hold the difference, so
		// anything beyond here is 1 exactly and the polynomial below
		// would only add noise.
		y = 1
	case z < 1:
		// erf(z) = 2/sqrt(pi) * sum (-1)^n z^(2n+1) / (n! (2n+1))
		//
		// Carried to z^21. Stopping at z^13 left a truncation error of
		// 1.3e-05 just below the split — the next term's size at z=1 —
		// which ate almost the whole parity budget on its own.
		const twoOverSqrtPi = 1.1283791670955126
		s := z * z
		p := float32(1.0 / 76204800.0)
		p = p*s - 1.0/6894720.0
		p = p*s + 1.0/685440.0
		p = p*s - 1.0/75600.0
		p = p*s + 1.0/9360.0
		p = p*s - 1.0/1320.0
		p = p*s + 1.0/216.0
		p = p*s - 1.0/42.0
		p = p*s + 1.0/10.0
		p = p*s - 1.0/3.0
		p = p*s + 1.0
		y = twoOverSqrtPi * z * p
	default:
		const (
			p  = 0.3275911
			a1 = 0.254829592
			a2 = -0.284496736
			a3 = 1.421413741
			a4 = -1.453152027
			a5 = 1.061405429
		)
		t := 1 / (1 + p*z)
		poly := float32(a5)
		poly = poly*t + a4
		poly = poly*t + a3
		poly = poly*t + a2
		poly = poly*t + a1
		y = 1 - poly*t*expf32(-z*z)
	}

	if neg {
		return -y
	}
	return y
}
