package binaries

import (
	"os"
	"strings"
	"sync"
)

// detectDistro returns the lowercase distro id from /etc/os-release
// (the standard ID field). Cached after first read because the value
// is constant for the process's lifetime.
var detectDistro = sync.OnceValue(func() string {
	raw, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "ID=") {
			continue
		}
		v := strings.TrimPrefix(line, "ID=")
		v = strings.Trim(v, "\"' ")
		return strings.ToLower(v)
	}
	return ""
})

// matchDistro reports whether the running host matches the operator
// declared distro hint. Empty hint matches any. Includes an
// id-like-id check so "debian" matches ubuntu/raspbian which
// share the debian package format (via /etc/os-release ID_LIKE).
func matchDistro(want string) bool {
	want = strings.ToLower(want)
	if want == "" {
		return true
	}
	id := detectDistro()
	if id == "" {
		return false
	}
	if id == want {
		return true
	}
	// Check ID_LIKE for compatible distros.
	raw, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "ID_LIKE=") {
			continue
		}
		v := strings.TrimPrefix(line, "ID_LIKE=")
		v = strings.Trim(v, "\"' ")
		for _, p := range strings.Fields(v) {
			if strings.ToLower(p) == want {
				return true
			}
		}
	}
	return false
}
