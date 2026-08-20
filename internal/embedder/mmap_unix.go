//go:build unix

package embedder

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// mapping is a read-only memory map of a checkpoint.
//
// WHY MAP RATHER THAN READ. multilingual-e5-base is 1.1 GB and bge-m3
// is 2.2 GB. Read into the Go heap, that is 1.1 GB the garbage
// collector must account for and the kernel can never reclaim, on a
// node that is also running raft, bbolt and an agent loop. Mapped, the
// same bytes live in the page cache: the kernel can evict them under
// pressure and page them back on demand, and a second node process
// sharing the same file shares the pages rather than duplicating them.
//
// It also halves peak memory at load. Reading meant holding the file
// bytes AND the parsed float slices simultaneously.
type mapping struct {
	data []byte
}

func mapFile(f *os.File) (*mapping, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size <= 0 {
		return nil, fmt.Errorf("embedder: %s is empty", f.Name())
	}
	if int64(int(size)) != size {
		return nil, fmt.Errorf("embedder: %s is too large to map on this platform", f.Name())
	}
	data, err := unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("embedder: mmap %s: %w", f.Name(), err)
	}
	// Advise sequential access: the loader walks the file once, in
	// order, and telling the kernel so lets it read ahead instead of
	// faulting a page at a time through a gigabyte.
	_ = unix.Madvise(data, unix.MADV_SEQUENTIAL)
	return &mapping{data: data}, nil
}

func (m *mapping) Close() error {
	if m == nil || m.data == nil {
		return nil
	}
	data := m.data
	m.data = nil
	return unix.Munmap(data)
}
