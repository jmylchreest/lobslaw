package gateway

import "github.com/jmylchreest/lobslaw/internal/compute"

// The button that grants a whole KIND of command rather than one
// command.
//
// The per-command scope buttons cannot help an agent that probes its
// environment: every probe is a different command line, so a grant
// naming one of them is never matched a second time, and the
// compound shape of a probe means no grant can name it at all. The
// user is then offered exactly one answer — Approve — over and over,
// which is how a confirmation becomes a reflex.
//
// A tier is nameable even when the command is not. So the offer is
// "everything of this kind, here, until this conversation ends".

// riskGrantOffered reports whether a tier may be granted by tapping.
//
// Read and write only. Network, destructive and unknown are absent
// deliberately and there is no configuration that adds them: "allow
// all destructive commands here" is not a decision anybody should be
// able to make with one tap in a chat window, and "allow everything I
// could not read" is not a decision at all. An operator who genuinely
// wants either writes a policy rule, where it is visible in
// `lobslaw policy list` and revocable.
func riskGrantOffered(tier compute.CommandRisk) bool {
	switch tier {
	case compute.RiskRead, compute.RiskWrite:
		return true
	default:
		return false
	}
}

// riskGrantLabel is the button's text.
//
// It says what the grant COVERS rather than what the command is: the
// user is being asked about a class, and a button reading "Approve for
// this chat" beside one reading "Allow read-only here" has to make the
// difference obvious at a glance.
func riskGrantLabel(tier compute.CommandRisk) string {
	switch tier {
	case compute.RiskRead:
		return "Allow read-only here"
	case compute.RiskWrite:
		return "Allow local writes here"
	default:
		return ""
	}
}

// keyboardRows lays inline buttons out two to a row.
//
// A single strip of five buttons renders on a phone as five slivers of
// truncated text, and the whole point of the tier button is that
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
