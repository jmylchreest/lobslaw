package gateway

import (
	"github.com/jmylchreest/lobslaw/internal/commandrisk"
	"strings"
)

// The button that grants a whole KIND of command rather than one
// command.
//
// The per-command scope buttons cannot help an agent that probes its
// environment: every probe is a different command line, so a grant
// naming one of them is never matched a second time, and the compound
// shape of a probe means no grant can name it at all. The user is then
// offered exactly one answer — Approve — over and over, which is how a
// confirmation becomes a reflex.
//
// A label is nameable even when the command is not. So the offer is
// "everything of this kind, here, until this conversation ends".

// grantableLabels are the labels a tap may approve.
//
// Reads and writes only. Deletes, disrupts, network, privilege and
// unreadable are absent deliberately and there is no configuration that
// adds them: "allow all deletions here" is not a decision anybody
// should be able to make with one tap in a chat window, and "allow
// everything I could not read" is not a decision at all. An operator
// who genuinely wants either writes a policy rule, where it is visible
// in `lobslaw policy list` and revocable.
var grantableLabels = map[commandrisk.RiskLabel]bool{
	commandrisk.LabelReads:  true,
	commandrisk.LabelWrites: true,
}

// riskGrantOffered reports whether a tap may approve this command's
// whole label set.
//
// EVERY label must be grantable. A command that reads and deletes
// offers nothing, because approving "the kind of thing this is" would
// approve the deletion — and a button whose text is a summary of two
// things is a button nobody reads carefully.
func riskGrantOffered(labels []commandrisk.RiskLabel) bool {
	if len(labels) == 0 {
		return false
	}
	for _, l := range labels {
		if !grantableLabels[l] {
			return false
		}
	}
	return true
}

// riskGrantLabel is the button's text.
//
// It names what the grant COVERS rather than what the command is: the
// user is being asked about a class, and a button reading "Approve for
// this chat" beside one reading "Allow reads + writes here" has to make
// the difference obvious at a glance.
func riskGrantLabel(labels []commandrisk.RiskLabel) string {
	if !riskGrantOffered(labels) {
		return ""
	}
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, string(l))
	}
	return "Allow " + strings.Join(parts, " + ") + " here"
}

// keyboardRows lays inline buttons out two to a row.
//
// A single strip of five buttons renders on a phone as five slivers of
// truncated text, and the whole point of the label button is that
// somebody can tell it apart from the one next to it before tapping.
func keyboardRows(buttons []map[string]string) [][]map[string]string {
	const perRow = 2
	rows := make([][]map[string]string, 0, (len(buttons)+perRow-1)/perRow)
	for i := 0; i < len(buttons); i += perRow {
		end := min(i+perRow, len(buttons))
		rows = append(rows, buttons[i:end])
	}
	return rows
}
