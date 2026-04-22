package types

// Retention governs how long a memory record lives before it is
// a candidate for consolidation or pruning.
type Retention string

const (
	RetentionSession  Retention = "session"
	RetentionEpisodic Retention = "episodic"
	RetentionLongTerm Retention = "long-term"
)

func (r Retention) IsValid() bool {
	switch r {
	case RetentionSession, RetentionEpisodic, RetentionLongTerm:
		return true
	}
	return false
}
