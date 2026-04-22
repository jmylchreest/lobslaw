//go:build unix

package discovery

import "syscall"

// setSoBroadcast enables SO_BROADCAST on a Unix socket file descriptor.
// `syscall.SetsockoptInt` on Unix takes an int fd — on Windows it takes
// `syscall.Handle`, hence the platform split.
func setSoBroadcast(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
}
