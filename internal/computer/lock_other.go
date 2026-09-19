//go:build !linux

package computer

import "os"

func lockRoot(string) (*os.File, error) { return nil, ErrUnavailable }
