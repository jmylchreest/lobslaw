//go:build unix

package sandbox

import (
	"os"
	"syscall"
)

// statNlink extracts st_nlink from a FileInfo on Unix. Returns false
// on platforms (or file types) where Nlink isn't available.
func statNlink(info os.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Nlink), true
}
