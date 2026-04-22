//go:build !unix

package sandbox

import "os"

func statNlink(_ os.FileInfo) (uint64, bool) { return 0, false }
