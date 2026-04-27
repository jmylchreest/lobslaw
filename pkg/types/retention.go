package types

import (
	"fmt"
	"strings"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// ParseRetention turns the user-facing string form ("session" /
// "episodic" / "long-term") into the proto enum. Empty input maps
// to RETENTION_UNSPECIFIED, which the service layer treats as "no
// filter" on read paths and "default to episodic" on write paths.
//
// Use this at every boundary that takes user input — CLI args, JSON
// request bodies, tool-call argument maps. Internal callers building
// proto records should use the lobslawv1.Retention_RETENTION_* enum
// constants directly so the compiler binds the value.
func ParseRetention(s string) (lobslawv1.Retention, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return lobslawv1.Retention_RETENTION_UNSPECIFIED, nil
	case "session":
		return lobslawv1.Retention_RETENTION_SESSION, nil
	case "episodic":
		return lobslawv1.Retention_RETENTION_EPISODIC, nil
	case "long-term", "long_term", "longterm":
		return lobslawv1.Retention_RETENTION_LONG_TERM, nil
	}
	return lobslawv1.Retention_RETENTION_UNSPECIFIED,
		fmt.Errorf("retention: %q must be session | episodic | long-term", s)
}

// RetentionString returns the user-facing string for an enum value.
// Inverse of ParseRetention; used for JSON output and log lines so
// readers see "long-term" instead of "RETENTION_LONG_TERM".
func RetentionString(r lobslawv1.Retention) string {
	switch r {
	case lobslawv1.Retention_RETENTION_SESSION:
		return "session"
	case lobslawv1.Retention_RETENTION_EPISODIC:
		return "episodic"
	case lobslawv1.Retention_RETENTION_LONG_TERM:
		return "long-term"
	}
	return ""
}
