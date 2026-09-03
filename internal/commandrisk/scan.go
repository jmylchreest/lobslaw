package commandrisk

import (
	"strings"
	"unicode"
)

// Reading a command line into the commands it runs.
//
// Deliberately MORE permissive than NormaliseCommand, and the
// difference is the question being asked. That one asks "is there a
// stable name a grant could be written against", so a glob defeats it.
// This asks "what does it do", and a glob does not change the answer —
// `ls *.go` still reads and `rm *` still deletes.
//
// What it does refuse is anything that introduces a program it has not
// read: command substitution, backticks, a variable in the command
// slot, a subshell.

type riskToken struct {
	text string
	// expands records that the token carried a $ outside single
	// quotes, so its value is not what is written. Only consulted in
	// positions the classifier reads — argv[0] and a subcommand — since
	// elsewhere an expansion adds arguments to a program already
	// identified.
	expands bool
}

type riskSegment struct {
	raw    string
	tokens []riskToken
	// unreadable is a reason code, set when something in this segment
	// defeats static reading.
	unreadable string
	// writeTargets are the paths this segment redirects into, other
	// than /dev/null and file-descriptor dups. Kept as tokens rather
	// than a bool so `> /etc/passwd` and `> /tmp/probe` can be told
	// apart.
	writeTargets []riskToken
}

// splitRiskSegments breaks a command line into the commands it runs.
//
// ok=false only for input that is not a command line at all: an
// unterminated quote, or a control character. Everything else produces
// segments, some of which may be marked unreadable.
func splitRiskSegments(cmd string) ([]riskSegment, bool) {
	sc := &riskScanner{runes: []rune(cmd)}
	if !sc.scan() {
		return nil, false
	}
	return sc.segs, true
}

// scan runs the walk. false means the input is not a command line.
func (s *riskScanner) scan() bool {
	for i := 0; i < len(s.runes); i++ {
		r := s.runes[i]
		if !riskReadableRune(r) {
			return false
		}
		if r == '\'' || r == '"' {
			q, ok := scanQuoted(s.runes, i)
			if !ok {
				return false
			}
			s.addQuoted(q)
			i = q.end
			continue
		}
		// Order is load-bearing. Substitution and redirect handling run
		// BEFORE the inside-a-substitution check, so `$(` still opens a
		// depth and `)` still closes one; everything after it is inert
		// while a substitution is open.
		if next, handled := s.scanUnreadable(r, i); handled {
			i = next
			continue
		}
		if next, handled := s.scanRedirect(r, i); handled {
			i = next
			continue
		}
		if s.subDepth > 0 || s.backtick {
			s.write(r)
			continue
		}
		if next, handled := s.scanSeparator(r, i); handled {
			i = next
			continue
		}
		s.write(r)
	}
	s.flushSegment(len(s.runes))
	return true
}

// scanUnreadable handles the runes that mean this segment cannot be
// read statically: an escape, a substitution, a subshell.
func (s *riskScanner) scanUnreadable(r rune, i int) (int, bool) {
	switch r {
	case '\\':
		// An escape changes what the next character means in ways the
		// token text would not preserve.
		s.cur.unreadable = "escaped_command"
		s.started = true
	case '`':
		s.cur.unreadable = "command_substitution"
		s.backtick = !s.backtick
		s.started = true
	case '$':
		if i+1 < len(s.runes) && s.runes[i+1] == '(' {
			s.cur.unreadable = "command_substitution"
			s.subDepth++
			s.started = true
			return i + 1, true
		}
		s.expands = true
		s.tok.WriteRune(r)
		s.started = true
	case '(', ')':
		if r == ')' && s.subDepth > 0 {
			s.subDepth--
			return i, true
		}
		// A subshell runs its contents somewhere this classifier is not
		// reading.
		s.cur.unreadable = "subshell"
		s.started = true
	default:
		return i, false
	}
	return i, true
}

// scanRedirect handles ">", "<" and bash's "&>".
func (s *riskScanner) scanRedirect(r rune, i int) (int, bool) {
	at := i
	switch {
	case r == '>' || r == '<':
		// A bare number in front of a redirect is a file descriptor,
		// not an argument: `apt-get --version 2>&1` passes no operand
		// called "2", and treating it as one would send the subcommand
		// lookup hunting for it.
		if s.started && allDigits(s.tok.String()) {
			s.tok.Reset()
			s.started, s.expands = false, false
		}
	case r == '&' && i+1 < len(s.runes) && s.runes[i+1] == '>':
		at = i + 1 // "&>file": stdout and stderr to one place
	default:
		return i, false
	}
	s.flushToken()
	return consumeRedirect(s.runes, at, &s.cur), true
}

// scanSeparator handles whitespace and the operators that end a
// command: newline, ";", "&", "|", "&&", "||".
func (s *riskScanner) scanSeparator(r rune, i int) (int, bool) {
	switch r {
	case ' ', '\t':
		s.flushToken()
		return i, true
	case '\n', ';':
		s.flushSegment(i)
		s.segStart = i + 1
		return i, true
	case '&', '|':
		end, next := i, i
		if i+1 < len(s.runes) && s.runes[i+1] == r {
			next++ // "&&" or "||"
		}
		s.flushSegment(end)
		s.segStart = next + 1
		return next, true
	}
	return i, false
}

// addQuoted folds a quoted run into the token being built.
func (s *riskScanner) addQuoted(q quotedSpan) {
	if q.substitutes {
		s.cur.unreadable = "command_substitution"
	}
	if q.expands {
		s.expands = true
	}
	s.tok.WriteString(q.text)
	s.started = true
}

// write appends an ordinary rune to the token being built.
func (s *riskScanner) write(r rune) {
	s.tok.WriteRune(r)
	s.started = true
}

func (s *riskScanner) flushToken() {
	if s.started {
		s.cur.tokens = append(s.cur.tokens, riskToken{text: s.tok.String(), expands: s.expands})
		s.tok.Reset()
		s.started, s.expands = false, false
	}
}

func (s *riskScanner) flushSegment(end int) {
	s.flushToken()
	s.cur.raw = strings.TrimSpace(string(s.runes[s.segStart:end]))
	if s.cur.raw != "" || len(s.cur.tokens) > 0 {
		s.segs = append(s.segs, s.cur)
	}
	s.cur = riskSegment{}
}

// riskReadableRune rejects the runes that make one string display as
// another.
//
// A segment that DISPLAYS as one command and IS another is consent
// obtained by misdirection, and the prompt quotes this text back at the
// user. Same refusal NormaliseCommand makes, for the same reason.
func riskReadableRune(r rune) bool {
	if unicode.IsControl(r) && r != '\n' && r != '\t' {
		return false
	}
	return !IsInvisible(r) && !(r > unicode.MaxASCII && unicode.IsSpace(r))
}

// consumeRedirect reads a redirection starting at the ">" or "<" in
// runes[i], recording on seg whether it writes somewhere real, and
// returns the index of the last rune consumed.
func consumeRedirect(runes []rune, i int, seg *riskSegment) int {
	reading := runes[i] == '<'
	j := i + 1
	if j < len(runes) && (runes[j] == '>' || runes[j] == '<') {
		j++ // ">>" append, "<<" heredoc
	}
	for j < len(runes) && (runes[j] == ' ' || runes[j] == '\t') {
		j++
	}
	start := j
	// A leading "&" is a file-descriptor dup ("2>&1"), not the
	// backgrounding operator, so it belongs to the target rather than
	// ending it.
	if j < len(runes) && runes[j] == '&' {
		j++
	}
	for j < len(runes) && !strings.ContainsRune(" \t\n;|&()<>", runes[j]) {
		j++
	}
	target := string(runes[start:j])
	switch {
	case reading:
		// Reading from a file changes nothing.
	case strings.HasPrefix(target, "&"):
		// A file-descriptor dup: `2>&1` writes nowhere new.
	case target == "/dev/null" || target == "/dev/stdout" || target == "/dev/stderr":
		// The conventional way to say "discard", and the reason a
		// probe full of `2>/dev/null` is not reported as a write.
	case target == "":
		seg.unreadable = "unreadable_redirect"
	default:
		seg.writeTargets = append(seg.writeTargets, riskToken{
			text:    target,
			expands: strings.Contains(target, "$"),
		})
	}
	return j - 1
}

// allDigits reports whether s is a bare number, which in front of a
// redirect means a file descriptor.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// indexRune finds r at or after start, or -1.
func indexRune(runes []rune, start int, r rune) int {
	for i := start; i < len(runes); i++ {
		if runes[i] == r {
			return i
		}
	}
	return -1
}
