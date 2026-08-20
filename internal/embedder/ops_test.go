package embedder

import (
	"math"
	"testing"
)

func TestLayerNormProducesZeroMeanUnitVariance(t *testing.T) {
	t.Parallel()
	const n = 8
	x := []float32{1, 2, 3, 4, 5, 6, 7, 8}
	w, b := make([]float32, n), make([]float32, n)
	for i := range w {
		w[i] = 1
	}
	layerNorm(x, w, b, 1, n, 1e-12)

	var mean, varc float64
	for _, v := range x {
		mean += float64(v)
	}
	mean /= n
	for _, v := range x {
		varc += (float64(v) - mean) * (float64(v) - mean)
	}
	varc /= n
	if math.Abs(mean) > 1e-5 {
		t.Errorf("mean = %v, want ~0", mean)
	}
	if math.Abs(varc-1) > 1e-4 {
		t.Errorf("variance = %v, want ~1", varc)
	}
}

// The BIASED variance (divide by n) is what PyTorch uses. With n-1 the
// output is scaled by sqrt(n/(n-1)) — 6.9% at n=8, under 0.07% at
// n=768. Right where a small-n test catches it and a realistic-width
// test does not, which is why this one is deliberately narrow.
func TestLayerNormUsesTheBiasedVariance(t *testing.T) {
	t.Parallel()
	const n = 4
	x := []float32{1, 2, 3, 4}
	w, b := []float32{1, 1, 1, 1}, []float32{0, 0, 0, 0}
	layerNorm(x, w, b, 1, n, 0)
	// mean 2.5, biased variance 1.25, so the first element is
	// (1-2.5)/sqrt(1.25) = -1.341641
	if math.Abs(float64(x[0])-(-1.3416407)) > 1e-5 {
		t.Errorf("x[0] = %v, want -1.3416407 (biased); -1.161895 would mean n-1", x[0])
	}
}

func TestLayerNormAppliesScaleAndShift(t *testing.T) {
	t.Parallel()
	x := []float32{1, 2, 3, 4}
	w := []float32{2, 2, 2, 2}
	b := []float32{5, 5, 5, 5}
	layerNorm(x, w, b, 1, 4, 0)
	// Normalised values are symmetric about 0, so with a constant
	// scale and shift the mean must land exactly on the bias.
	var mean float64
	for _, v := range x {
		mean += float64(v)
	}
	if mean /= 4; math.Abs(mean-5) > 1e-5 {
		t.Errorf("mean after scale+shift = %v, want 5", mean)
	}
}

// The exact erf GELU, checked against values that distinguish it from
// the tanh approximation. They agree to ~3 decimals, so the check has
// to be tighter than that to mean anything.
func TestGeluIsTheExactErfForm(t *testing.T) {
	t.Parallel()
	x := []float32{-3, -1, 0, 1, 3}
	want := []float64{-0.00404969, -0.15865525, 0, 0.84134475, 2.99595031}
	gelu(x)
	for i := range x {
		if d := math.Abs(float64(x[i]) - want[i]); d > 1e-6 {
			t.Errorf("gelu(%d) = %v, want %v (diff %.2e — tanh approx would differ here)", i, x[i], want[i], d)
		}
	}
}

func TestSoftmaxRowsSumToOne(t *testing.T) {
	t.Parallel()
	x := []float32{1, 2, 3, 0, 0, 0}
	softmaxRows(x, 2, 3)
	for r := range 2 {
		var sum float32
		for _, v := range x[r*3 : (r+1)*3] {
			sum += v
		}
		if math.Abs(float64(sum)-1) > 1e-6 {
			t.Errorf("row %d sums to %v, want 1", r, sum)
		}
	}
}

// Large scores are the reason the max is subtracted. Without it exp
// overflows to +Inf and the row becomes NaN — which then propagates
// through every later layer.
func TestSoftmaxSurvivesLargeScores(t *testing.T) {
	t.Parallel()
	x := []float32{1000, 1001, 999}
	softmaxRows(x, 1, 3)
	var sum float32
	for _, v := range x {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			t.Fatalf("softmax of large scores produced %v", v)
		}
		sum += v
	}
	if math.Abs(float64(sum)-1) > 1e-6 {
		t.Errorf("sum = %v, want 1", sum)
	}
}

func TestMeanPoolAveragesEveryRow(t *testing.T) {
	t.Parallel()
	h := []float32{1, 2, 3, 4, 5, 6} // 3 rows of 2
	got := meanPool(h, 3, 2)
	if got[0] != 3 || got[1] != 4 {
		t.Errorf("meanPool = %v, want [3 4]", got)
	}
}

func TestClsPoolTakesTheFirstRowOnly(t *testing.T) {
	t.Parallel()
	h := []float32{1, 2, 3, 4, 5, 6}
	got := clsPool(h, 2)
	if got[0] != 1 || got[1] != 2 {
		t.Errorf("clsPool = %v, want [1 2]", got)
	}
}

// A zero vector must survive normalisation. The empty-string fixture
// reaches here, and dividing by a zero norm gives NaN that poisons
// every cosine the vector is later compared against.
func TestL2NormLeavesAZeroVectorAlone(t *testing.T) {
	t.Parallel()
	v := []float32{0, 0, 0}
	for _, x := range l2norm(v) {
		if math.IsNaN(float64(x)) {
			t.Fatal("l2norm turned a zero vector into NaN")
		}
	}
}

func TestL2NormGivesUnitLength(t *testing.T) {
	t.Parallel()
	v := l2norm([]float32{3, 4})
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if math.Abs(math.Sqrt(sum)-1) > 1e-6 {
		t.Errorf("norm = %v, want 1", math.Sqrt(sum))
	}
}

// RoBERTa and XLM-RoBERTa reserve rows up to pad_token_id; plain BERT
// starts at 0. Getting this wrong reads real but wrong embeddings.
func TestPositionOffsetPerFamily(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		modelType string
		pad, want int
	}{
		{"bert", 0, 0},
		{"bert", 1, 0},
		{"roberta", 1, 2},
		{"xlm-roberta", 1, 2},
		{"xlm-roberta", -1, 0},
	} {
		if got := positionOffset(tc.modelType, tc.pad); got != tc.want {
			t.Errorf("positionOffset(%q, %d) = %d, want %d", tc.modelType, tc.pad, got, tc.want)
		}
	}
}
