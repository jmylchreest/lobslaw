package main

import (
	"os"
	"strings"
	"testing"
)

// Go's flag package STOPS at the first non-flag argument, so a flag
// written after a positional is never parsed — it lands in fs.Args()
// and the command treats it as another positional.
//
// Live, that looked like this:
//
//	$ lobslaw learned approve "skill:Prepare Release Notes" --context full
//	approved skill:Prepare Release Notes
//	--context: not found: --context
//	full: not found: full
//	learned approve: 2 of 3 could not be approved
//
// It approved the right thing and then reported two failures for
// flags. `session search lobster --context prod` was worse: the flag
// became part of the QUERY, so it searched for "lobster --context
// prod" and found nothing, on a cluster it never contacted.
//
// parseFlagsAndPositionals exists for exactly this and re-parses the
// tail after each positional. This test asserts nobody reaches for
// fs.Args() instead — it is the third time this shape has been
// fixed, after `cluster export-operator` and `trace`.
func TestNoCommandReadsPositionalsFromFsArgs(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.Contains(line, "fs.Args()") || strings.HasPrefix(trimmed, "//") {
				continue
			}
			// parseFlagsAndPositionals is the fix, and it necessarily
			// calls fs.Args() itself.
			if name == "dispatch.go" {
				continue
			}
			offenders = append(offenders,
				name+":"+itoa(i+1)+"  "+strings.TrimSpace(line))
		}
	}
	if len(offenders) > 0 {
		t.Errorf("these read positionals from fs.Args(), so a flag after a positional "+
			"is silently taken as one — use parseFlagsAndPositionals:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
