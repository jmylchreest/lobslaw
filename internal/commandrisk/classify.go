package commandrisk

import (
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

// What a command DOES, as distinct from what it is CALLED.
//
// The per-command approval gate asks about every shell command,
// because until now nothing in the system had an opinion about what
// any of them do. That is defensible for `rm -rf /` and indefensible
// for `id && uname -a && df -h`, and the cost of not distinguishing
// them is not merely noise: an operator asked eight times in four
// minutes stops reading the prompt, and a confirmation answered
// reflexively launders consent rather than obtaining it. The comments
// in approval.go and shell_key.go both say so; this is the part that
// acts on it.
//
// So a command is classified into a risk tier, and an approval mode
// decides which tiers are worth asking about. The classification is
// STATIC and FAIL-CLOSED: a program the table does not name is
// unknown, an argument the classifier cannot read makes the whole
// segment unknown, and unknown never auto-allows in any mode.
//
// Deliberately NOT an embedding or a similarity score. `rm -rf
// /tmp/build` and `ls -l /tmp/build` are neighbours in every text
// embedding, and the only property that justifies not asking is
// soundness — a nearest neighbour does not have one. Where a model is
// wanted it answers a closed enum and is consulted separately; see
// command_risk_model.go.

const (
	// LabelReads inspects state and changes none of it.
	LabelReads RiskLabel = "reads"
	// LabelWrites creates, copies, appends to or edits, recoverably.
	LabelWrites RiskLabel = "writes"
	// LabelDeletes removes data. The distinction from disrupts is
	// RECOVERABILITY: a deletion is undone by a backup, or not at all.
	LabelDeletes RiskLabel = "deletes"
	// LabelDisrupts takes something down — a restarted service, a
	// killed process, an unmounted filesystem, a flushed firewall.
	// Undone by the opposite command, in seconds.
	LabelDisrupts RiskLabel = "disrupts"
	// LabelNetwork reaches off the box. Its own label because the blast
	// radius is somebody else's machine, and because egress is the
	// shape prompt injection wants.
	LabelNetwork RiskLabel = "network"
	// LabelPrivilege runs as root, or changes who may become root.
	LabelPrivilege RiskLabel = "privilege"
	// LabelUnreadable is what the classifier says when it cannot read
	// the command. The honest answer, and never approvable.
	LabelUnreadable RiskLabel = "unreadable"
)

// The reason vocabulary this replaces was a per-tier map whose
// destructive entry read "deletes_or_changes_machine_state" — an "or"
// in a category name, which is a category telling you it is two
// categories. The labels ARE the reason now, and `Why` below carries
// only the READING that produced them where that is not obvious from
// the program: a scratch path, a system path, an unreadable construct.

const (
	// scopeOrdinary is a path that is neither scratch nor system: the
	// project checkout, a data directory, somebody's work.
	scopeOrdinary pathScope = iota
	// scopeScratch is a declared throwaway root. Deleting inside one is
	// a write, not a loss.
	scopeScratch
	// scopeSystem is the machine itself.
	scopeSystem
	// scopeOpaque is a path whose value we cannot see — an expansion, a
	// substitution. `rm -rf $DIR` is not `rm -rf /tmp/x` and must never
	// be classified as though it were.
	scopeOpaque
)

// RiskSegment is one command in a compound command line.
type RiskSegment struct {
	// Raw is the segment as written, so the prompt can quote the exact
	// part that caused the ask rather than the whole line.
	Raw string `json:"raw"`
	// Program is the program name as invoked, with any path stripped.
	Program string `json:"program,omitempty"`
	// Via is the privilege wrapper the program ran under — sudo, doas,
	// pkexec — when there was one. Named separately because a headline
	// reading "destructive · true" for `sudo -n true` describes the
	// wrong half of what is happening.
	Via string `json:"via,omitempty"`
	// Labels is everything this segment does. Empty means an empty
	// segment — a trailing ";" — which the caller skips.
	Labels []RiskLabel `json:"labels"`
	// Why names the READING that produced the labels when it was not
	// simply the program's table entry: "scratch_path", "system_path",
	// "shell_keyword", "opaque_target". A closed set, never free text,
	// and empty when the labels speak for themselves.
	Why string `json:"why,omitempty"`
}

// RiskVerdict is the classification of a whole command line.
type RiskVerdict struct {
	// Labels is the union across every segment.
	//
	// A union rather than a maximum, which is the whole change: a
	// command that deletes AND reaches the network now reports both,
	// where the tier this replaced kept only the worst and the egress
	// vanished from the verdict entirely.
	Labels []RiskLabel `json:"labels"`
	// Programs is every program named, in order, without repeats. This
	// is what makes a 400-character probe legible in one line.
	Programs []string      `json:"programs,omitempty"`
	Segments []RiskSegment `json:"segments,omitempty"`
	// Why is the culprit segment's reading.
	Why string `json:"why,omitempty"`
	// Culprit is the segment carrying the severest label, and
	// CulpritIndex its 1-based position. Display only — nothing gates
	// on severity, and this exists so the prompt quotes the step that
	// caused the ask rather than the whole line.
	Culprit      string `json:"culprit,omitempty"`
	CulpritIndex int    `json:"culprit_index,omitempty"`
	// Unreadable counts segments the classifier could not read.
	Unreadable int `json:"unreadable,omitempty"`
	// FromModel records that a configured model contributed labels.
	FromModel bool `json:"from_model,omitempty"`
}

// Approved reports whether every label this command carries is one the
// operator approved.
//
// THE GATE, and deliberately a subset check rather than a comparison.
// Nothing is ranked: a command may run when everything it does was
// approved, and asks otherwise. LabelUnreadable is never in an
// approved set, so a command nobody could read always asks.
func (v RiskVerdict) Approved(approved map[RiskLabel]bool) bool {
	if len(v.Labels) == 0 {
		return false
	}
	for _, l := range v.Labels {
		// Refused by name, not merely by being absent from the set. An
		// operator cannot approve "everything I could not read" even by
		// writing it out, and ApprovedLabels rejects it at parse time
		// too — belt and braces, because this is the one label whose
		// approval would approve everything.
		if l == LabelUnreadable || !approved[l] {
			return false
		}
	}
	return true
}

// ClassifyRisk reads a command line and says what it does.
//
// Never returns an error: an input it cannot read is a verdict of
// L(LabelUnreadable), which is a real answer and the one that asks.
func ClassifyRisk(raw string) RiskVerdict {
	cmd := strings.TrimSpace(raw)
	if cmd == "" || !utf8.ValidString(cmd) {
		return RiskVerdict{Labels: L(LabelUnreadable), Why: "unreadable"}
	}
	segs, ok := splitRiskSegments(cmd)
	if !ok || len(segs) == 0 {
		return RiskVerdict{Labels: L(LabelUnreadable), Why: "unreadable"}
	}

	table := ActiveCommandRisks()
	var v RiskVerdict
	seen := map[string]bool{}
	worst := 0
	for _, seg := range segs {
		rs := classifyRiskSegment(seg, table)
		if len(rs.Labels) == 0 {
			continue // an empty segment: a trailing ";" or a stray "&&"
		}
		v.Segments = append(v.Segments, rs)
		v.Labels = MergeLabels(v.Labels, rs.Labels)
		if HasLabel(rs.Labels, LabelUnreadable) {
			v.Unreadable++
		}
		// The wrapper first, then the program it ran: `sudo`, `true`.
		// A shell keyword is not a program and listing `for`, `do`,
		// `done` alongside `id` and `uname` turns the one legible line
		// in the prompt back into noise.
		for _, name := range []string{rs.Via, rs.Program} {
			if name == "" || seen[name] || rs.Why == "shell_keyword" {
				continue
			}
			seen[name] = true
			v.Programs = append(v.Programs, name)
		}
		// Severity picks which segment the prompt quotes. It decides
		// nothing about whether the command runs.
		if sev := severityOf(rs.Labels); sev > worst {
			worst = sev
			v.Culprit, v.CulpritIndex, v.Why = rs.Raw, len(v.Segments), rs.Why
		}
	}
	if len(v.Segments) == 0 {
		return RiskVerdict{Labels: L(LabelUnreadable), Why: "unreadable"}
	}
	return v
}

// classifyRiskSegment reads one segment's argv.
func classifyRiskSegment(seg riskSegment, table map[string]CommandRiskRule) RiskSegment {
	out := RiskSegment{Raw: seg.raw}
	unreadable := func(program, why string) RiskSegment {
		out.Program, out.Labels, out.Why = program, L(LabelUnreadable), why
		return out
	}
	if seg.unreadable != "" {
		return unreadable("", seg.unreadable)
	}
	tokens := seg.tokens
	if len(tokens) == 0 {
		return out // no labels — the caller skips it
	}

	// Wrappers first, and bounded: a chain longer than this is not a
	// command anybody wrote by hand, and an unbounded loop over
	// attacker-shaped argv is not worth the elegance.
	var floor []RiskLabel
	for range 4 {
		if tokens[0].expands {
			return unreadable("", "variable_command")
		}
		name := programName(tokens[0])
		w, isWrapper := wrapperCommands[name]
		if !isWrapper {
			break
		}
		rest, ok := unwrap(tokens[1:], w)
		if !ok {
			// A wrapper with nothing to wrap is not unreadable, it is
			// the program itself: bare `env` prints the environment and
			// `nohup` alone is a usage message. Only a wrapper that
			// carries something it will not let us read — an
			// assignment prefix, an unresolvable tail — is unknown.
			if w.bareLabels != nil && !w.wrapsSomething(tokens[1:]) {
				out.Labels, out.Why = w.bareLabels, ""
				return out
			}
			return unreadable(name, "unreadable_wrapper")
		}
		if w.root {
			// Privilege is ADDED, not substituted. `sudo rm -rf /` is a
			// deletion and a privilege escalation, and reporting only
			// one of them describes half of what is happening.
			floor = MergeLabels(floor, L(LabelPrivilege))
			out.Via = name
		}
		tokens = rest
	}

	if tokens[0].expands {
		return unreadable("", "variable_command")
	}
	name := programName(tokens[0])
	out.Program = name

	if ReservedWords[name] {
		// `for`, `while`, `if`, `time`. The body is not parsed; see the
		// non-goal in the design. Reported distinctly so the prompt can
		// say "shell loop" rather than "unrecognised command: for".
		return unreadable(name, "shell_keyword")
	}

	rule, found := table[name]
	if !found {
		// A program an operator declared as reaching off the box counts
		// even when it is not in the risk table — but this package does
		// not know what a policy action is, and must not: the
		// dependency runs one way. internal/compute translates its
		// command classes into table entries at wiring time, so the
		// operator still says it once.
		return unreadable(name, "unrecognised_command")
	}

	labels := rule.Labels
	if len(labels) == 0 {
		labels = L(LabelUnreadable)
	}
	out.Why = rule.Why
	args := tokens[1:]

	verb, why := verbLabels(rule, labels, args)
	if why != "" {
		return unreadable(name, why)
	}
	labels = verb

	for _, tok := range args {
		for pattern, esc := range rule.Escalate {
			if escalateMatches(pattern, tok.text) {
				labels = MergeLabels(labels, esc)
			}
		}
	}
	if len(seg.writeTargets) > 0 {
		// A redirection writes whatever the program on the left prints,
		// so the segment writes however innocent that program is.
		// `echo pwned > ~/.ssh/authorized_keys` is the case that makes
		// this non-negotiable.
		labels = MergeLabels(labels, L(LabelWrites))
	}

	// Only a targeting program's own operands are read as paths. For
	// anything else the operands are inputs — `grep root /etc/passwd`
	// reads a system file and changes nothing — so the only paths that
	// count are the ones being redirected into.
	var operands []riskToken
	if rule.Targets {
		operands = targetOperands(args, rule.TargetLast)
	}
	if len(operands) > 0 || len(seg.writeTargets) > 0 {
		labels, out.Why = applyTargets(labels, rule, operands, seg.writeTargets)
	}

	out.Labels = MergeLabels(labels, floor)
	return out
}

// programName is the command as invoked, without its path:
// /usr/bin/ssh is ssh, the same reading ClassifyCommand takes.
func programName(t riskToken) string {
	name := t.text
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// targetOperands returns the non-flag arguments a targeting program
// acts on: all of them, or only the last when the rest are sources.
func targetOperands(args []riskToken, lastOnly bool) []riskToken {
	var operands []riskToken
	for _, a := range args {
		if strings.HasPrefix(a.text, "-") {
			continue
		}
		operands = append(operands, a)
	}
	if lastOnly && len(operands) > 1 {
		return operands[len(operands)-1:]
	}
	return operands
}

// firstOperand returns the first non-flag argument.
// verbLabels resolves the labels a rule's VERB selects — a flag for
// pacman, a word for git, an operand's presence for mount — leaving the
// base labels alone when the rule names no verbs.
//
// Split out of classifyRiskSegment because it is the one part with
// three independent dispatch shapes, and inlining all three put that
// function over the complexity the linter allows for a reason.
//
// A non-empty second return is a refusal: the verb was there and
// nobody had catalogued it, which is unreadable rather than whatever
// the base happened to say.
func verbLabels(rule CommandRiskRule, base []RiskLabel, args []riskToken) ([]RiskLabel, string) {
	switch {
	case len(rule.FlagSub) > 0:
		flag, ok := firstFlag(args)
		if !ok {
			return base, "" // bare invocation prints usage
		}
		named, found := rule.FlagSub[flag]
		if !found {
			return nil, "unrecognised_flag"
		}
		return named, ""

	case len(rule.Sub) > 0:
		sub, expands, ok := firstOperand(args)
		switch {
		case !ok:
			return base, "" // `git` on its own prints usage
		case expands:
			return nil, "variable_subcommand"
		}
		named, found := rule.Sub[sub]
		if !found {
			return nil, "unrecognised_subcommand"
		}
		return named, ""

	case len(rule.OperandLabels) > 0:
		if _, _, ok := firstOperand(args); ok {
			return MergeLabels(base, rule.OperandLabels), ""
		}
	}
	return base, ""
}

// firstFlag returns the first token that looks like a flag.
//
// The first one only: `pacman -S --noconfirm foo` is a sync, and the
// tokens after it modify how rather than what. A program whose meaning
// changes on a LATER flag wants Escalate, which is additive, on top.
func firstFlag(args []riskToken) (string, bool) {
	for _, a := range args {
		if !strings.HasPrefix(a.text, "-") || a.text == "-" || a.text == "--" {
			continue
		}
		if a.expands {
			return "", false
		}
		return a.text, true
	}
	return "", false
}

func firstOperand(args []riskToken) (text string, expands, ok bool) {
	for _, a := range args {
		if strings.HasPrefix(a.text, "-") {
			continue
		}
		return a.text, a.expands, true
	}
	return "", false, false
}

// unwrap drops a wrapper's own flags and operands, returning the
// command it runs. ok=false when there is nothing left, or when the
// wrapper carries something that changes what the command means.
func unwrap(args []riskToken, w wrapperSpec) ([]riskToken, bool) {
	skipped := 0
	for i := 0; i < len(args); i++ {
		tok := args[i].text
		if strings.HasPrefix(tok, "-") {
			if w.valueFlags[tok] {
				i++
			}
			continue
		}
		if w.refuseAssign && IsEnvAssignment(tok) {
			return nil, false
		}
		if skipped < w.skipOperands {
			skipped++
			continue
		}
		return args[i:], true
	}
	return nil, false
}

// escalateMatches compares an Escalate key against a token: exact, or
// prefix when the key ends in "*".
func escalateMatches(pattern, tok string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(tok, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == tok
}

// ---------------------------------------------------------------------
// Reading the command line.
//
// Deliberately MORE permissive than NormaliseCommand, and the
// difference is the question being asked. That one asks "is there a
// stable name a grant could be written against", so a glob defeats it:
// `rm *` names a different set of files every time it runs. This one
// asks "what does it do", and a glob does not change the answer —
// `ls *.go` still reads and `rm *` still deletes. Refusing globs here
// would make the classifier blind to commands it can classify
// perfectly well.
//
// What it does refuse is anything that introduces a program it has not
// read: command substitution, backticks, a variable in the command
// slot, a subshell.

// riskScanner walks a command line once, accumulating tokens into
// segments.
//
// A struct rather than a closure over locals, because the dispatch is
// wide — quotes, substitutions, redirects, separators — and each kind
// needs the same five accumulators. Sharing them through a receiver is
// what lets the loop body split into methods that are each readable on
// their own.
type riskScanner struct {
	runes []rune
	segs  []riskSegment
	cur   riskSegment

	tok     strings.Builder
	started bool
	expands bool

	segStart int
	// subDepth counts open "$(" and backtick reports an open backtick.
	// Inside either, the separators belong to a command this scanner is
	// not reading, so they must not split anything out here.
	subDepth int
	backtick bool
}

// quotedSpan is what a quoted run contributes to the token being built.
type quotedSpan struct {
	text string
	// expands records a $ that is still live inside the quotes.
	expands bool
	// substitutes records a $( or a backtick, which runs a program
	// this classifier has not read.
	substitutes bool
	// end is the index of the closing quote.
	end int
}

// scanQuoted reads the quoted run starting at runes[i].
//
// Single quotes are literal; double quotes are not, and that
// difference is the whole reason this is separate. Expansion and
// substitution both still happen inside double quotes, so quoting does
// NOT make `"$(rm -rf /)"` inert — a scanner that treated every quoted
// span as data would classify it as an ordinary argument.
//
// ok=false for an unterminated quote, which is not a command line.
func scanQuoted(runes []rune, i int) (quotedSpan, bool) {
	quote := runes[i]
	closing := indexRune(runes, i+1, quote)
	if closing < 0 {
		return quotedSpan{}, false
	}
	inner := runes[i+1 : closing]
	span := quotedSpan{text: string(inner), end: closing}
	if quote == '\'' {
		return span, true
	}
	for j := 0; j < len(inner); j++ {
		switch inner[j] {
		case '`':
			span.substitutes = true
		case '$':
			if j+1 < len(inner) && inner[j+1] == '(' {
				span.substitutes = true
			} else {
				span.expands = true
			}
		}
	}
	return span, true
}

// ---------------------------------------------------------------------
// The operator's table, and the tier on the request context.

// activeCommandRisks is the table in force, in the shape
// activeCommandClasses already uses.
var activeCommandRisks atomic.Pointer[map[string]CommandRiskRule]

// SetCommandRisks installs the operator's table, MERGED over the
// shipped one rather than replacing it.
//
// The opposite of SetCommandClasses, deliberately. That table has six
// entries and an operator can reasonably restate it; this one has
// hundreds, and replacing it wholesale would mean adding one in-house
// tool silently reclassified every command in the table as unknown —
// turning a small config edit into a flood of confirmations, which is
// the failure this whole change exists to remove. An entry with an
// empty tier removes the shipped one, which is how somebody says "stop
// classifying this".
func SetCommandRisks(m map[string]CommandRiskRule) {
	if len(m) == 0 {
		activeCommandRisks.Store(nil)
		return
	}
	merged := make(map[string]CommandRiskRule, len(DefaultCommandRisks)+len(m))
	for k, v := range DefaultCommandRisks {
		merged[k] = v
	}
	for k, v := range m {
		if len(v.Labels) == 0 && len(v.Sub) == 0 && len(v.Escalate) == 0 && len(v.OperandLabels) == 0 {
			delete(merged, k)
			continue
		}
		merged[k] = v
	}
	activeCommandRisks.Store(&merged)
}

// ActiveCommandRisks returns the table in force.
func ActiveCommandRisks() map[string]CommandRiskRule {
	if m := activeCommandRisks.Load(); m != nil {
		return *m
	}
	return DefaultCommandRisks
}
