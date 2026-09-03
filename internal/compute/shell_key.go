package compute

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jmylchreest/lobslaw/internal/commandrisk"
)

// What a shell approval is allowed to name.
//
// The tempting design is to generalise: derive "git status" from
// "git status --short" so one approval covers the family. It does not
// survive contact with real commands. Whether the tail is safe to drop
// depends entirely on the program — "ssh host" would cover
// "ssh host rm -rf ~", and the same shape recurs in docker run, npm
// run, find, xargs, env, timeout and sh -c. There is no rule about
// argv shape that separates the safe case from the dangerous one, and
// encoding it per-program is the config this feature exists to stop
// people writing.
//
// So a key names one command and never a class of them. Repetition is
// absorbed by the session grant, which covers the rest of the
// conversation; deliberate generalisation is an operator rule
// (resource = "git *"), where it is written down, visible in
// `lobslaw policy list`, and revocable.
//
// What is canonicalised is only whitespace and quoting, so that
// approving a command once does not leave the user asked again because
// the model spelled it with different spacing. Nothing is ever dropped.
//
// The refusals below are not a denylist of dangerous commands — the
// hardline floor does that, and policy does the rest. They are the set
// of commands that have no stable identity for a grant to name: if
// what runs depends on the environment (expansion, globbing,
// substitution) or on more than one program (compound commands), then
// no key can describe it and the honest answer is to ask every time.

// shellKeyDisplayMax bounds a key that can be shown in full. Past it
// the command is confirmable but not grantable: consent to a command
// you were only shown the first 300 characters of is not consent.
const shellKeyDisplayMax = 300

// shellSafeToken is the set a token can be rendered bare in. Anything
// else is single-quoted, so the rendering is unambiguous and stable.
const shellSafeToken = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=@,+%"

// NormaliseCommand canonicalises a command into the exact string a
// grant will name, or refuses to derive one.
//
// ok=false is not an error. It means this command has no stable
// identity a reusable grant could describe, so it is asked about every
// time and no scope button is offered.
func NormaliseCommand(raw string) (string, bool) {
	cmd := strings.TrimSpace(raw)
	if cmd == "" {
		return "", false
	}
	if !utf8.ValidString(cmd) {
		return "", false
	}
	// Whole-string refusals first, so a character that would change
	// what runs can never survive into a token — including inside
	// quotes, where it is easy to believe it is inert.
	for _, r := range cmd {
		switch {
		case r == '\t':
			// Folded to a space below rather than refused: a tab
			// between arguments changes nothing about what runs.
		case unicode.IsControl(r):
			// Newlines, carriage returns, NUL. A key spanning two
			// lines is two commands wearing one name.
			return "", false
		case commandrisk.IsInvisible(r):
			// A key that DISPLAYS as "git status" but IS
			// "git‮status" would be consent obtained by
			// misdirection. The user cannot see the difference and the
			// engine cannot miss it.
			return "", false
		case r > unicode.MaxASCII && unicode.IsSpace(r):
			// NBSP and friends. A token with invisible whitespace in it
			// is a display lie for the same reason.
			return "", false
		case r == '$':
			// Expands inside double quotes too, so quoting does not
			// make it inert. What "echo $HOME" runs depends on the
			// environment, so no key describes it.
			return "", false
		case r == '`' || r == '\\':
			// Substitution and escaping. Both change what the tokens
			// mean in ways the re-rendering below would not preserve.
			return "", false
		case r == '#':
			// A comment, and the reason this is refused rather than
			// quoted: `ls #foo` runs `ls`, but re-rendering the token
			// yields `ls '#foo'`, which runs ls against a file called
			// "#foo". The key would then name a longer command than the
			// one that actually runs — so an approval given for
			// `git clean -fdx '#-n'` (which deletes nothing) would be
			// matched by `git clean -fdx #-n` (which deletes
			// everything untracked). The whole design rests on the key
			// and the command being the same operation.
			return "", false
		case r == '!':
			// History expansion in an interactive shell, and a reserved
			// word in front position: `! rm -rf x` negates rm's exit
			// status and still runs it, while the quoted `'!' rm -rf x`
			// is a command-not-found. One key, two behaviours.
			return "", false
		}
	}

	tokens, ok := shellTokens(cmd)
	if !ok || len(tokens) == 0 {
		return "", false
	}
	// A VAR=value prefix runs the command in a different environment,
	// so "PATH=/tmp git status" is not "git status" and must not
	// inherit its grant.
	if commandrisk.IsEnvAssignment(tokens[0]) {
		return "", false
	}
	// A reserved word in front position means the shell, not a program.
	// `time ls` and `'time' ls` render identically and do different
	// things — the first is the shell builtin, the second is
	// /usr/bin/time — so no single key describes both.
	if commandrisk.ReservedWords[tokens[0]] {
		return "", false
	}

	var b strings.Builder
	for i, tok := range tokens {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(renderToken(tok))
	}
	return b.String(), true
}

// shellTokens splits on whitespace honouring single and double quotes,
// refusing anything whose meaning depends on the shell rather than on
// the tokens themselves.
func shellTokens(cmd string) ([]string, bool) {
	var (
		tokens  []string
		cur     strings.Builder
		started bool
	)
	flush := func() {
		if started {
			tokens = append(tokens, cur.String())
			cur.Reset()
			started = false
		}
	}

	runes := []rune(cmd)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == ' ' || r == '\t':
			flush()
		case r == '\'' || r == '"':
			// Quoted spans are literal: the metacharacter refusals
			// below do not apply inside them, because inside them the
			// characters are data. That is what makes `grep '*.go'`
			// grantable while `rm *` is not.
			closing := -1
			for j := i + 1; j < len(runes); j++ {
				if runes[j] == r {
					closing = j
					break
				}
			}
			if closing < 0 {
				return nil, false // unterminated quote
			}
			cur.WriteString(string(runes[i+1 : closing]))
			started = true
			i = closing
		case strings.ContainsRune("&|;<>()", r):
			// Compound commands, pipelines, redirection, subshells.
			// More than one program runs, so one key cannot name it.
			return nil, false
		case strings.ContainsRune("*?[]{}~", r):
			// Globbing and expansion. "rm *" is a different operation
			// every time it runs, so there is nothing stable to grant.
			return nil, false
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	flush()
	return tokens, true
}

// renderToken emits a token bare when it is unambiguous, and
// single-quoted otherwise, so that every distinct token list has
// exactly one rendering and every rendering re-reads as the same
// token list.
func renderToken(tok string) string {
	if tok == "" {
		return "''"
	}
	bare := true
	for _, r := range tok {
		if !strings.ContainsRune(shellSafeToken, r) {
			bare = false
			break
		}
	}
	if bare {
		return tok
	}
	return "'" + strings.ReplaceAll(tok, "'", `'\''`) + "'"
}

// hasNonASCII reports whether a key carries runes that could be
// mistaken for ASCII ones.
func hasNonASCII(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return true
		}
	}
	return false
}
