package httpbody

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type countingReader struct{ n int }

func (r *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	r.n += len(p)
	return len(p), nil
}
func TestReadLimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, input string
		large       bool
	}{
		{"empty", "", false}, {"below", "abc", false}, {"exact", "abcd", false}, {"above", "abcde", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Read(strings.NewReader(tc.input), 4)
			if tc.large {
				if !errors.Is(err, ErrTooLarge) || got != nil {
					t.Fatalf("got %q, %v", got, err)
				}
				return
			}
			if err != nil || string(got) != tc.input {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
	r := new(countingReader)
	if _, err := Read(r, 1024); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if r.n != 1025 {
		t.Fatalf("read %d bytes", r.n)
	}
}

type failedReader struct{ err error }

func (r failedReader) Read([]byte) (int, error) { return 0, r.err }
func TestReadPreservesError(t *testing.T) {
	t.Parallel()
	want := io.ErrUnexpectedEOF
	if _, err := Read(failedReader{want}, 4); !errors.Is(err, want) {
		t.Fatal(err)
	}
}
