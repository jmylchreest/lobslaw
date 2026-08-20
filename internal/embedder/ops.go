package embedder

import "math"

// The element-wise stages. Each one is small, and each one has a
// variant that is nearly right and silently worse — which is why they
// are separate functions with their own tests rather than inlined
// into the forward pass.

// layerNorm normalises each row of x[m,n] to zero mean and unit
// variance, then applies the learned scale and shift.
//
// eps goes INSIDE the sqrt: sqrt(var + eps), never sqrt(var) + eps.
// Both produce finite, plausible numbers. The wrong one shifts every
// activation slightly, and the only symptom is an embedding that is
// a bit worse than it should be.
//
// The variance is the BIASED estimator (divide by n, not n-1), which
// is what PyTorch's LayerNorm uses.
func layerNorm(x, weight, bias []float32, m, n int, eps float32) {
	for i := range m {
		row := x[i*n : (i+1)*n]

		var mean float64
		for _, v := range row {
			mean += float64(v)
		}
		mean /= float64(n)

		var variance float64
		for _, v := range row {
			d := float64(v) - mean
			variance += d * d
		}
		variance /= float64(n)

		inv := float32(1 / math.Sqrt(variance+float64(eps)))
		for j := range row {
			row[j] = (row[j]-float32(mean))*inv*weight[j] + bias[j]
		}
	}
}

// gelu applies the EXACT, erf-based GELU in place.
//
//	x * 0.5 * (1 + erf(x / sqrt(2)))
//
// HuggingFace's "gelu" is this, NOT the tanh approximation that many
// implementations reach for because it is faster. The two agree to
// about three decimal places, so substituting the approximation
// produces embeddings that are subtly and permanently wrong while
// every test that checks for "reasonable numbers" still passes.
// config.json says hidden_act=gelu; this is what that means.
func gelu(x []float32) {
	for i, v := range x {
		x[i] = float32(float64(v) * 0.5 * (1 + math.Erf(float64(v)/math.Sqrt2)))
	}
}

// softmaxRows applies a numerically-stable softmax to each row of
// x[m,n].
//
// The max is subtracted before exponentiating. Attention scores reach
// magnitudes where a bare exp overflows to +Inf, and the resulting
// NaN propagates through the whole vector — the one failure here that
// is not silent, but only because it is catastrophic.
func softmaxRows(x []float32, m, n int) {
	for i := range m {
		row := x[i*n : (i+1)*n]

		maxV := row[0]
		for _, v := range row[1:] {
			if v > maxV {
				maxV = v
			}
		}

		var sum float32
		for j, v := range row {
			e := float32(math.Exp(float64(v - maxV)))
			row[j] = e
			sum += e
		}
		if sum == 0 {
			continue
		}
		inv := 1 / sum
		for j := range row {
			row[j] *= inv
		}
	}
}

// meanPool averages every token's hidden state.
//
// Averages ALL rows it is given, with no attention mask, which is
// correct here only because this encoder never pads: sequences are
// processed one at a time at their true length. A batched
// implementation must mask, or padding tokens drag every vector
// toward the padding embedding.
func meanPool(h []float32, rows, dim int) []float32 {
	out := make([]float32, dim)
	if rows == 0 {
		return out
	}
	for i := range rows {
		row := h[i*dim : (i+1)*dim]
		for j := range dim {
			out[j] += row[j]
		}
	}
	inv := 1 / float32(rows)
	for j := range out {
		out[j] *= inv
	}
	return out
}

// clsPool takes the first token's hidden state.
//
// Which pooling a checkpoint wants is DECLARED, in
// 1_Pooling/config.json — bge uses CLS, e5 uses mean. Guessing gives
// a working embedder that is quietly worse than the model it loaded.
func clsPool(h []float32, dim int) []float32 {
	out := make([]float32, dim)
	copy(out, h[:dim])
	return out
}

// l2norm scales v to unit length, in place, returning it.
//
// A zero vector is returned unchanged rather than divided by zero:
// the empty-string fixture reaches here, and NaN would propagate into
// every cosine it was ever compared against.
func l2norm(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
	return v
}
