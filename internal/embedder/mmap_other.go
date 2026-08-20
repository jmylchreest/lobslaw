//go:build !unix

package embedder

import (
	"fmt"
	"io"
	"os"
)

// mapping falls back to reading the whole file where mmap is not
// available.
//
// Correct but memory-hungry: the weights live on the Go heap rather
// than in the page cache, so a 1.1 GB model costs 1.1 GB the collector
// must track and the kernel cannot reclaim. The zero-copy tensor path
// still works — it aliases this buffer instead of a mapping — so only
// the memory characteristics differ, never the numbers.
type mapping struct {
	data []byte
}

func mapFile(f *os.File) (*mapping, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() <= 0 {
		return nil, fmt.Errorf("embedder: %s is empty", f.Name())
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return &mapping{data: data}, nil
}

func (m *mapping) Close() error {
	if m != nil {
		m.data = nil
	}
	return nil
}
