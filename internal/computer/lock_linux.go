//go:build linux

package computer

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func lockRoot(root string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(root, ".controller.lock"), os.O_CREATE|os.O_RDWR, privateFileMode)
	if err != nil {
		return nil, fmt.Errorf("computer controller lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: another controller owns this browser root", ErrUnavailable)
	}
	return f, nil
}
