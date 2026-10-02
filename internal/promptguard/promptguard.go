// Package promptguard scans text that is destined for a prompt.
//
// It exists because several paths write content the model later reads
// as context: episodic ingest stores user messages verbatim, memory
// builtins write whatever the model was told to remember, and SOUL
// fragments and skill manifests are loaded from disk. Each is a place
// where a poisoned record can be planted once and replayed into every
// later turn.
//
// The output is advisory. A finding marks a record for quarantine, it
// does not delete it: a false positive on a dropped record is
// undebuggable, and the record is usually the evidence someone wants.
// Recall skips quarantined records; an operator can still list them.
//
// Detection is deliberately conservative. The cost of a miss is one
// suspicious record reaching a prompt that already wraps it as
// untrusted; the cost of a false positive is a real memory silently
// missing from recall. Where those trade off, this errs toward
// missing.
package promptguard

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/jmylchreest/lobslaw/pkg/promptgen"
)

// Detector names the rule that fired. Stored on the record, so it has
// to stay stable — an operator filtering quarantined records by
// detector is filtering on these strings.
type Detector string

const (
	DetectorInvisible   Detector = "invisible-unicode"
	DetectorDelimiter   Detector = "delimiter-fragment"
	DetectorInstruction Detector = "instruction-shape"
	DetectorExfil       Detector = "exfil-shape"
)

// Finding is one rule firing, with enough detail to judge it without
// re-running the scan.
type Finding struct {
	Detector Detector
	Detail   string
}

func (f Finding) String() string { return string(f.Detector) + ": " + f.Detail }

// Scan returns every finding in s, in detector order. An empty result
// means nothing fired.
func Scan(s string) []Finding {
	if s == "" {
		return nil
	}
	var out []Finding
	for _, check := range []func(string) *Finding{
		scanInvisible, scanDelimiters, scanInstructions, scanExfil,
	} {
		if f := check(s); f != nil {
			out = append(out, *f)
		}
	}
	return out
}

// Suspicious is the boolean form, for callers that only decide
// whether to quarantine.
func Suspicious(s string) (Finding, bool) {
	if f := Scan(s); len(f) > 0 {
		return f[0], true
	}
	return Finding{}, false
}

// --- invisible characters --------------------------------------------

// Zero-width joiners are NOT flagged on their own. They are load
// bearing in ordinary text — every family emoji and every flag is a
// ZWJ sequence — so treating them as hostile would quarantine a
// cheerful message about someone's kids. What is flagged is the
// unambiguous set: characters with no legitimate role in prose that
// a person typed.
func scanInvisible(s string) *Finding {
	var softHyphens int
	// Tag characters are legitimate in exactly one place: a subdivision
	// flag, which is a base 🏴 followed by tag letters and a cancel
	// tag. Flagging the block outright would quarantine any message
	// containing a Scottish or Welsh flag, so the sequences are
	// stripped before the scan and anything left is smuggling.
	s = stripFlagTagSequences(s)
	for _, r := range s {
		switch {
		case r == '\u200B', r == '\u200C', r == '\uFEFF':
			return &Finding{DetectorInvisible, fmt.Sprintf("zero-width character U+%04X", r)}
		case r >= '\u202A' && r <= '\u202E', r >= '\u2066' && r <= '\u2069':
			// Bidi overrides reorder how text DISPLAYS without changing
			// what the model reads, so a human reviewing a record can
			// see something different from what is stored.
			return &Finding{DetectorInvisible, fmt.Sprintf("bidirectional override U+%04X", r)}
		case r >= '\U000E0000' && r <= '\U000E007F':
			// The tag block encodes ASCII invisibly. Its only current
			// use in the wild is smuggling instructions.
			return &Finding{DetectorInvisible, fmt.Sprintf("Unicode tag character U+%04X", r)}
		case r == '\u00AD':
			softHyphens++
		case unicode.Is(unicode.Cf, r) && r != '\u200D':
			return &Finding{DetectorInvisible, fmt.Sprintf("format character U+%04X", r)}
		}
	}
	// A soft hyphen or two is a typesetting artefact from a paste; a
	// run of them is a word broken up to evade a substring match.
	if softHyphens >= suspiciousSoftHyphenCount {
		return &Finding{DetectorInvisible, fmt.Sprintf("%d soft hyphens", softHyphens)}
	}
	return nil
}

// --- delimiter fragments ---------------------------------------------

// A record cannot close a delimiter it is wrapped in unless it
// contains one.
//
// The WRAPPER TAGS come from promptgen, which writes them. Keeping a
// second copy here was two authorities for one fact: adding a wrapper
// tag there would leave this scanner silently not covering it, and
// nothing would fail.
//
// The CONTROL TOKENS below are this package's own, because nothing in
// promptgen emits them — they are what the model's chat template
// honours, which is a different concern that happens to need the same
// treatment.
var controlTokens = []string{
	"</relevant_context",
	"<|im_start|>",
	"<|im_end|>",
	"<|system|>",
	"<|endoftext|>",
	"[INST]",
	"[/INST]",
}

func delimiterFragments() []string {
	return append(promptgen.Delimiters(), controlTokens...)
}

func scanDelimiters(s string) *Finding {
	lower := strings.ToLower(s)
	for _, frag := range delimiterFragments() {
		if strings.Contains(lower, strings.ToLower(frag)) {
			return &Finding{DetectorDelimiter, "contains " + frag}
		}
	}
	return nil
}

// --- instruction shapes ----------------------------------------------

// Phrases that only make sense as an instruction to a model. Anchored
// loosely enough to survive rewording, tight enough not to fire on
// someone discussing the topic — though a conversation ABOUT prompt
// injection will trip these, which is the accepted cost of quarantine
// rather than deletion.
var instructionShapes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bignore\s+(all\s+)?(previous|prior|above|earlier)\s+(instructions?|prompts?|rules?)`),
	regexp.MustCompile(`(?i)\bdisregard\s+(all\s+)?(previous|prior|above|earlier)\s+(instructions?|prompts?|rules?)`),
	regexp.MustCompile(`(?i)\byou\s+are\s+now\s+(a|an|the)\b`),
	// Requires the phrase in an instruction position. Bare "new system
	// prompt" appears in ordinary technical conversation — someone
	// mentioning a course on the subject is not an attack.
	regexp.MustCompile(`(?i)\b(your|here\s+is\s+(your|the)|following)\s+new\s+system\s+prompt\b`),
	regexp.MustCompile(`(?i)\bnew\s+system\s+prompt\s*:`),
	regexp.MustCompile(`(?i)\b(system|developer)\s*:\s*(you|ignore|always|never)\b`),
	regexp.MustCompile(`(?i)\bfrom\s+now\s+on,?\s+(you|always|never)\b`),
}

func scanInstructions(s string) *Finding {
	for _, re := range instructionShapes {
		if m := re.FindString(s); m != "" {
			return &Finding{DetectorInstruction, "instruction-shaped phrase: " + strings.TrimSpace(m)}
		}
	}
	return nil
}

// --- exfiltration shapes ---------------------------------------------

var (
	secretPathRe = regexp.MustCompile(`(?i)(\.env\b|id_rsa\b|\.ssh/|credentials\.json|\.aws/credentials|secrets?\.(ya?ml|json|toml))`)
	urlRe        = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>]+`)
	pipeToNetRe  = regexp.MustCompile(`(?i)\b(curl|wget)\b[^\n|]*\|`)
	tokenShapeRe = regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{16,}|sk-[A-Za-z0-9]{16,}|xox[baprs]-[A-Za-z0-9-]{10,})\b`)
)

// scanExfil looks for the COMBINATION that matters. A path to a
// secret is ordinary in a technical conversation, and so is a URL;
// the two adjacent, or a fetch piped somewhere, is the shape of an
// instruction to send a secret out.
func scanExfil(s string) *Finding {
	if tokenShapeRe.MatchString(s) && urlRe.MatchString(s) {
		return &Finding{DetectorExfil, "credential-shaped token alongside a URL"}
	}
	if secretPathRe.MatchString(s) && urlRe.MatchString(s) {
		return &Finding{DetectorExfil, "path to a secret alongside a URL"}
	}
	if pipeToNetRe.MatchString(s) && (secretPathRe.MatchString(s) || tokenShapeRe.MatchString(s)) {
		return &Finding{DetectorExfil, "network fetch piped alongside a secret"}
	}
	return nil
}

// --- redaction --------------------------------------------------------

var redactPatterns = []*regexp.Regexp{
	tokenShapeRe,
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{16,}=*`),
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
}

// Redact replaces credential-shaped substrings with a marker.
//
// For tool errors and MCP output on their way to a log or to the
// model: a failing command frequently echoes the argument that
// failed, and that argument is sometimes the key. Truncating the
// whole message would lose the error; replacing the secret keeps it
// readable.
func Redact(s string) string {
	for _, re := range redactPatterns {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	return s
}

// stripFlagTagSequences removes well-formed subdivision-flag tag
// sequences: 🏴 followed by tag letters and terminated by the cancel
// tag. Anything else in the tag block survives to be flagged.
func stripFlagTagSequences(s string) string {
	const (
		flagBase  = '\U0001F3F4'
		tagLow    = '\U000E0020'
		tagHigh   = '\U000E007E'
		tagCancel = '\U000E007F'
	)
	if !strings.ContainsRune(s, flagBase) {
		return s
	}
	var b strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if runes[i] != flagBase {
			b.WriteRune(runes[i])
			continue
		}
		j := i + 1
		for j < len(runes) && runes[j] >= tagLow && runes[j] <= tagHigh {
			j++
		}
		if j > i+1 && j < len(runes) && runes[j] == tagCancel {
			// A complete sequence: drop it entirely, base included.
			i = j
			continue
		}
		b.WriteRune(runes[i])
	}
	return b.String()
}

// --- quarantine marking -----------------------------------------------

// TagPrefix marks a record the scanner flagged.
//
// R5 proposed metadata["promptguard"], but EpisodicRecord carries no
// metadata map — only tags. A tag needs no schema change and no
// migration, and tags are already the filter mechanism everywhere
// else, so recall skipping quarantined records is one predicate
// rather than a new index.
const TagPrefix = "promptguard:"

// Tag renders a finding as a record tag.
func Tag(f Finding) string { return TagPrefix + string(f.Detector) }

// IsQuarantined reports whether any tag marks this record.
func IsQuarantined(tags []string) bool {
	for _, t := range tags {
		if strings.HasPrefix(t, TagPrefix) {
			return true
		}
	}
	return false
}
