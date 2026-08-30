package tools

// Compiled-in defaults for arguments the model may omit.
//
// A tool's default is part of its contract: the ToolDef description
// tells the model what happens when it leaves an argument out, and
// that sentence and the code have to agree. Naming them puts the two
// within reach of each other.
const (
	// DefaultCouncilMode has each reviewer answer without seeing the
	// others. Independent first, because reviewers who can read each
	// other converge, and the point of asking several is disagreement.
	DefaultCouncilMode = "independent"

	// DefaultScheduleNotifyOn notifies only when a scheduled run
	// matches its condition. The alternative is a message every run,
	// which trains the user to ignore the channel.
	DefaultScheduleNotifyOn = "match"

	// DefaultWebSearchType lets the provider choose between keyword
	// and neural search per query, which beats us guessing from a
	// query string we have not read.
	DefaultWebSearchType = "auto"
)
