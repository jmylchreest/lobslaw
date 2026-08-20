package embedder

// Batch and long-input handling.
//
// Both exist because the single-sequence Embed is the wrong shape for
// two real cases: a backfill embedding thousands of records, and a
// memory longer than the model's context.

// EmbedBatch encodes several id sequences.
//
// SEQUENTIAL, deliberately. Every Embed already parallelises its own
// matmuls across GOMAXPROCS, so running items concurrently on top of
// that oversubscribes the machine: N goroutines each spawning N more,
// all contending at the same join barriers. The work is identical
// either way and the flat version is faster.
//
// It also avoids padding. A batched forward pass would have to pad
// every sequence to the longest one and mask the padding out of the
// attention and the pooling — and a mean-pool that forgets the mask
// drags every short sequence's vector toward the padding embedding,
// silently. One sequence at a time has no padding to forget.
func (m *Model) EmbedBatch(seqs [][]int32) [][]float32 {
	out := make([][]float32, len(seqs))
	for i, ids := range seqs {
		out[i] = m.Embed(ids)
	}
	return out
}

// EmbedLong encodes a sequence of any length by chunking.
//
// Embed truncates at the model's context limit, which for
// multilingual-e5-base is 512 tokens — perhaps two paragraphs. A
// memory longer than that would have its tail silently discarded, and
// "silently" is the problem: recall would simply never match anything
// said in the second half.
//
// Chunks are combined by a LENGTH-WEIGHTED mean of the unnormalised
// vectors, then normalised once at the end. Weighting matters: an
// unweighted mean lets a three-token final chunk count as much as the
// 512-token chunk before it. Weighted, the result approximates
// mean-pooling over the whole sequence, which is what the model would
// have produced had its context been long enough.
//
// bos and eos are the model's boundary ids, passed in rather than
// guessed: each chunk must be wrapped as the model expects, and this
// package deliberately knows nothing about tokenizers. Pass -1 for
// either to omit it.
func (m *Model) EmbedLong(ids []int32, bos, eos int32) []float32 {
	body := stripBoundaries(ids, bos, eos)
	room := m.maxSeq - boundaryCount(bos, eos)
	if room < 1 {
		return m.Embed(ids)
	}
	if len(body) <= room {
		return m.Embed(ids)
	}

	acc := make([]float32, m.cfg.Hidden)
	var total float32
	for start := 0; start < len(body); start += room {
		end := min(start+room, len(body))
		chunk := wrapBoundaries(body[start:end], bos, eos)

		// The chunk's own pooled vector, before normalisation: an
		// already-normalised vector has lost the magnitude that makes
		// the weighting meaningful, so this re-derives it from the
		// hidden states rather than calling Embed.
		h, rows := m.hiddenStates(chunk)
		if rows == 0 {
			continue
		}
		var v []float32
		if m.pool == PoolCLS {
			v = clsPool(h, m.cfg.Hidden)
		} else {
			v = meanPool(h, rows, m.cfg.Hidden)
		}
		w := float32(end - start)
		for j := range acc {
			acc[j] += v[j] * w
		}
		total += w
	}
	if total == 0 {
		return acc
	}
	for j := range acc {
		acc[j] /= total
	}
	return l2norm(acc)
}

func boundaryCount(bos, eos int32) int {
	n := 0
	if bos >= 0 {
		n++
	}
	if eos >= 0 {
		n++
	}
	return n
}

// stripBoundaries removes the caller's boundary ids so chunking does
// not scatter them through the middle of the sequence.
func stripBoundaries(ids []int32, bos, eos int32) []int32 {
	if len(ids) > 0 && bos >= 0 && ids[0] == bos {
		ids = ids[1:]
	}
	if len(ids) > 0 && eos >= 0 && ids[len(ids)-1] == eos {
		ids = ids[:len(ids)-1]
	}
	return ids
}

// wrapBoundaries re-adds them around one chunk.
func wrapBoundaries(chunk []int32, bos, eos int32) []int32 {
	out := make([]int32, 0, len(chunk)+2)
	if bos >= 0 {
		out = append(out, bos)
	}
	out = append(out, chunk...)
	if eos >= 0 {
		out = append(out, eos)
	}
	return out
}
