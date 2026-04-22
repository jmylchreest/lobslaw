//go:build windows

package discovery

import "syscall"

// setSoBroadcast enables SO_BROADCAST on a Windows socket handle. The
// Windows `syscall.SetsockoptInt` takes `syscall.Handle` (not `int`)
// for the socket argument — the only meaningful difference from the
// Unix version.
func setSoBroadcast(fd uintptr) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
}
