package embedder

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"unsafe"
)

// nativeLittleEndian reports whether f32 in memory matches the
// little-endian layout safetensors uses on disk.
//
// Only when they agree can a tensor alias the mapping instead of being
// converted. Go still targets big-endian machines (s390x, some mips),
// where the same bytes mean different numbers — so this is checked
// rather than assumed, and the big-endian path simply copies.
var nativeLittleEndian = func() bool {
	x := uint32(1)
	return *(*byte)(unsafe.Pointer(&x)) == 1
}()

// The safetensors container: an 8-byte little-endian header length, a
// JSON header mapping tensor name to dtype/shape/byte-range, then the
// raw tensor bytes.
//
// Chosen over pickle for the obvious reason — a .bin checkpoint is a
// Python pickle, which is arbitrary code execution on load. A model
// file is something a user downloads from the internet, so a format
// that cannot execute anything is the only defensible option.

// maxHeaderLen bounds the JSON header before it is read.
//
// The length prefix is the first thing in an untrusted file and is
// used to size an allocation. Without a bound, eight crafted bytes
// ask for an arbitrary allocation and the process dies before any
// validation runs.
const maxHeaderLen = 64 << 20

type tensorInfo struct {
	DType   string `json:"dtype"`
	Shape   []int  `json:"shape"`
	Offsets [2]int `json:"data_offsets"`
}

// safetensors is an open checkpoint. Tensors are read on demand
// rather than all at once: the file is streamed into the float slices
// that are actually kept, so peak memory is one copy of the weights
// rather than the file plus the weights.
type safetensors struct {
	f       *os.File
	m       *mapping
	index   map[string]tensorInfo
	dataOff int64
}

func openSafetensors(path string) (*safetensors, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	// Mapped first, then parsed out of the mapping. Reading the length
	// prefix through the file handle and the rest through the map
	// would be two views of the same bytes that could disagree about
	// where the data starts.
	m, err := mapFile(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	fail := func(format string, args ...any) (*safetensors, error) {
		_ = m.Close()
		_ = f.Close()
		return nil, fmt.Errorf(format, args...)
	}
	if len(m.data) < 8 {
		return fail("safetensors: %s is too short to hold a header length", path)
	}
	n := binary.LittleEndian.Uint64(m.data[:8])
	// The length prefix is the first thing in an untrusted file and
	// decides how much is parsed as JSON. Bounded before it is used.
	if n == 0 || n > maxHeaderLen {
		return fail("safetensors: header length %d out of range (max %d)", n, maxHeaderLen)
	}
	if uint64(len(m.data)) < 8+n {
		return fail("safetensors: file is shorter than its own header claims")
	}
	var index map[string]tensorInfo
	if err := json.Unmarshal(m.data[8:8+n], &index); err != nil {
		return fail("safetensors: parse header: %w", err)
	}
	// __metadata__ is a free-form string map, not a tensor; leaving it
	// in makes every "unknown tensor" error message misleading.
	delete(index, "__metadata__")
	return &safetensors{f: f, m: m, index: index, dataOff: int64(8 + n)}, nil
}

func (s *safetensors) Close() error {
	err := s.m.Close()
	if cerr := s.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// tensor reads one F32 tensor by name.
//
// Only F32 is accepted, and an unsupported dtype is an ERROR rather
// than a best-effort conversion. A bf16 checkpoint silently
// reinterpreted as f32 would load, run, and produce noise — the exact
// class of failure this package is built to make impossible.
func (s *safetensors) tensor(name string) ([]float32, []int, error) {
	info, ok := s.index[name]
	if !ok {
		return nil, nil, fmt.Errorf("safetensors: no tensor %q", name)
	}
	if info.DType != "F32" {
		return nil, nil, fmt.Errorf("safetensors: tensor %q has dtype %s, only F32 is supported", name, info.DType)
	}
	size := info.Offsets[1] - info.Offsets[0]
	if size < 0 || size%4 != 0 {
		return nil, nil, fmt.Errorf("safetensors: tensor %q has byte length %d, not a whole number of f32", name, size)
	}
	want := 1
	for _, d := range info.Shape {
		want *= d
	}
	if size/4 != want {
		return nil, nil, fmt.Errorf("safetensors: tensor %q shape %v implies %d values but the byte range holds %d",
			name, info.Shape, want, size/4)
	}

	start := s.dataOff + int64(info.Offsets[0])
	if start < 0 || start+int64(size) > int64(len(s.m.data)) {
		return nil, nil, fmt.Errorf("safetensors: tensor %q runs past the end of the file", name)
	}
	buf := s.m.data[start : start+int64(size)]

	// ZERO COPY when the layout already matches: the returned slice
	// aliases the mapping, so a 1.1 GB checkpoint costs no heap at all
	// and the kernel keeps the pages evictable.
	//
	// Conditional on ALIGNMENT as well as endianness. safetensors pads
	// its header so tensors land on 8-byte boundaries in every file
	// seen so far, but the format does not guarantee it, and a
	// misaligned float32 slice is undefined behaviour rather than a
	// slow path. Checked, not assumed.
	if nativeLittleEndian && uintptr(unsafe.Pointer(&buf[0]))%4 == 0 {
		return unsafe.Slice((*float32)(unsafe.Pointer(&buf[0])), size/4), info.Shape, nil
	}
	out := make([]float32, size/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return out, info.Shape, nil
}

// has reports whether a tensor is present, for optional weights.
func (s *safetensors) has(name string) bool {
	_, ok := s.index[name]
	return ok
}
