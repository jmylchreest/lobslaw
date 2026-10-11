package logging

import (
	"context"
	"io"
	"log"
	"log/slog"
	"net/url"
	"regexp"
	"strings"

	logfilter "github.com/jmylchreest/slog-logfilter"
)

var customCredentialKey = regexp.MustCompile(`(?i)^(x[_ -]?api[_ -]?key|x[_ -]?auth[_ -]?token|x[_ -]?amz[_ -]?security[_ -]?token|bot[_ -]?token)$`)

// One immutable policy serves logs and errors returned across API boundaries.
// Secrets are recognized by field/context/shape; no credential cache is kept.
var sanitizer = newSanitizer()

func newSanitizer() *logfilter.Sanitiser {
	// Parse custom query names before text rules can rewrite the URL.
	rules := []logfilter.SanitiserRule{
		{Pattern: `(?i)https?://[^\s<>"']+`, ReplaceFunc: sanitiseCustomCredentialURL},
	}
	rules = append(rules, logfilter.DefaultSanitiserRules()...)
	rules = append(rules,
		logfilter.SanitiserRule{KeyPattern: customCredentialKey.String()},
		logfilter.SanitiserRule{Pattern: `/bot[0-9]+(?::|%3[aA])[A-Za-z0-9_-]+`},
		logfilter.SanitiserRule{Pattern: `\b[0-9]{5,}:[A-Za-z0-9_-]{20,}\b`},
		logfilter.SanitiserRule{Pattern: `\b(?:gh[pousr]_[A-Za-z0-9]{16,}|sk-[A-Za-z0-9_-]{16,}|xox[baprs]-[A-Za-z0-9-]{10,})\b`},
	)
	s, err := logfilter.NewSanitiserWithConfig(logfilter.SanitiserConfig{
		Rules:           rules,
		SanitiseObjects: true,
		SanitiseKeys:    true,
	})
	// Only programmer-authored constant rules are compiled here, like MustCompile.
	if err != nil {
		panic("logging: invalid sanitiser policy: " + err.Error())
	}
	return s
}

// Field rules do not apply to URL query keys. Preserve the additional keys
// recognised by our policy, including percent-encoded query names.
func sanitiseCustomCredentialURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return logfilter.Redaction
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return logfilter.Redaction
	}
	changed := false
	for key := range query {
		if customCredentialKey.MatchString(key) {
			query.Set(key, logfilter.Redaction)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	u.RawQuery = query.Encode()
	return u.String()
}

func SanitizeText(s string) string { return sanitizer.SanitiseText(s) }
func SafeError(err error) error    { return logfilter.SanitiseError(err, sanitizer) }

// StandardLogger forwards standard-library diagnostics through the supplied
// logger's handler, preserving the single filtering/sanitization pipeline.
func StandardLogger(l *slog.Logger, level slog.Level) *log.Logger {
	l = OrDefault(l)
	return slog.NewLogLogger(l.Handler(), level)
}

// Writer adapts complete diagnostic writes to the shared logger. It retains no
// buffered fragments and is intended for line-oriented library/flag diagnostics.
func Writer(l *slog.Logger, level slog.Level) io.Writer {
	l = OrDefault(l)
	return diagnosticWriter{logger: l, level: level}
}

type diagnosticWriter struct {
	logger *slog.Logger
	level  slog.Level
}

func (w diagnosticWriter) Write(p []byte) (int, error) {
	w.logger.Log(context.Background(), w.level, SanitizeText(strings.TrimSpace(string(p))))
	return len(p), nil
}

// OrDefault preserves an injected pipeline. Before application logger setup,
// nil callers still get mandatory redaction around the existing default sink.
// This does not install a logger or replace the live filter configuration.
func OrDefault(l *slog.Logger) *slog.Logger {
	if l != nil {
		return l
	}
	return slog.New(logfilter.NewSanitisingHandler(slog.Default().Handler(), sanitizer))
}
