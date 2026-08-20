package embedder

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
)

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
	index   map[string]tensorInfo
	dataOff int64
}

func openSafetensors(path string) (*safetensors, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	var lenBuf [8]byte
	if _, err := io.ReadFull(f, lenBuf[:]); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("safetensors: read header length: %w", err)
	}
	n := binary.LittleEndian.Uint64(lenBuf[:])
	if n == 0 || n > maxHeaderLen {
		_ = f.Close()
		return nil, fmt.Errorf("safetensors: header length %d out of range (max %d)", n, maxHeaderLen)
	}
	raw := make([]byte, n)
	if _, err := io.ReadFull(f, raw); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("safetensors: read header: %w", err)
	}
	var index map[string]tensorInfo
	if err := json.Unmarshal(raw, &index); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("safetensors: parse header: %w", err)
	}
	// __metadata__ is a free-form string map, not a tensor; leaving it
	// in makes every "unknown tensor" error message misleading.
	delete(index, "__metadata__")
	return &safetensors{f: f, index: index, dataOff: int64(8 + n)}, nil
}

func (s *safetensors) Close() error { return s.f.Close() }

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

	buf := make([]byte, size)
	if _, err := s.f.ReadAt(buf, s.dataOff+int64(info.Offsets[0])); err != nil {
		return nil, nil, fmt.Errorf("safetensors: read %q: %w", name, err)
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
