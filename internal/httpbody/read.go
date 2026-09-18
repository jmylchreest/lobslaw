// Package httpbody bounds buffered HTTP response bodies before decoding.
package httpbody

import (
	"errors"
	"io"
)

var ErrTooLarge = errors.New("response body exceeds size limit")

// Read accepts at most limit bytes. The extra byte distinguishes an exact-fit
// response from a truncated response whose prefix might still be valid JSON.
// The caller owns and closes r; oversized bodies are not drained.
func Read(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(body)) > limit {
		return nil, ErrTooLarge
	}
	if err != nil {
		return nil, err
	}
	return body, nil
}
