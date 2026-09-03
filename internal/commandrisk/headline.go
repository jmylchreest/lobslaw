package commandrisk

import (
	"fmt"
	"strings"
)

// The one line that leads a confirmation prompt.

// riskProgramsShown bounds the program list in a headline. Past this
// the list stops being a summary and becomes another wall.
const riskProgramsShown = 8

// RiskHeadline is the one line that leads a confirmation prompt.
//
// It answers, in order, the three things somebody about to tap Approve
// needs: what KIND of thing this is, WHICH part made it that, and what
// it touches. The verbatim command still follows — this is added to
// it, never instead of it.
func RiskHeadline(v RiskVerdict) string {
	if len(v.Labels) == 0 {
		return ""
	}
	var b strings.Builder
	// The label set leads, severest first. A command that deletes AND
	// reaches the network says both — which is the point of the set,
	// and what a single tier could not tell you.
	b.WriteString(RenderLabels(v.Labels))

	switch {
	case HasLabel(v.Labels, LabelUnreadable) && v.Unreadable > 0 && len(v.Segments) > 1:
		fmt.Fprintf(&b, " · %d of %d steps unreadable (%s)",
			v.Unreadable, len(v.Segments), v.Why)
	case v.CulpritIndex > 0 && len(v.Segments) > 1 && severityOf(v.Labels) > labelSeverity[LabelReads]:
		// Naming the step is the largest readability win there is: in a
		// 300-character probe, one `rm` is why the question is being
		// asked and the other eight steps are noise.
		fmt.Fprintf(&b, " · `%s` (step %d of %d)", v.Culprit, v.CulpritIndex, len(v.Segments))
	case v.Why != "":
		b.WriteString(" · " + v.Why)
	}

	if len(v.Programs) > 0 {
		b.WriteString(" · ")
		if len(v.Programs) > riskProgramsShown {
			b.WriteString(strings.Join(v.Programs[:riskProgramsShown], ", "))
			fmt.Fprintf(&b, " +%d more", len(v.Programs)-riskProgramsShown)
		} else {
			b.WriteString(strings.Join(v.Programs, ", "))
		}
	}
	if v.FromModel {
		// Said out loud, because a label a model contributed is a
		// different kind of claim from one read off the argv.
		b.WriteString(" · model")
	}
	return b.String()
}
