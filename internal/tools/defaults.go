package tools

import "time"

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

// Tool limits are separate contracts even when their numeric values coincide.
const (
	defaultMemorySearchLimit     int           = 5
	maxMemorySearchLimit         int           = 20
	defaultMemoryRecentLimit     int           = 20
	maxMemoryRecentLimit         int           = 50
	defaultDreamRecapLimit       int           = 10
	maxDreamRecapLimit           int           = 50
	defaultSlackReadLimit        int           = 50
	maxSlackReadLimit            int           = 200
	defaultSlackSearchLimit      int           = 10
	maxSlackSearchLimit          int           = 25
	defaultSessionListLimit      int           = 10
	maxSessionListLimit          int           = 50
	solverResponseGrace          time.Duration = 5 * time.Second
	solverDialTimeout            time.Duration = 10 * time.Second
	writeApplyTimeout            time.Duration = 5 * time.Second
	councilRoundTimeout          time.Duration = 45 * time.Second
	remoteTransferStderrMaxBytes int64         = 8 << 10
	defaultPDFTimeout            time.Duration = 120 * time.Second
	pdfMaxCompletionTokens       int           = 4096
	DefaultFetchCacheTTL         time.Duration = 10 * time.Minute
	DefaultFetchCacheSize        int           = 64
	fetchLinkTextMaxChars        int           = 200
	fetchDiagnosticMaxBytes      int           = 256
	maxTrackedPinnedFailures     int           = 512
	scannerMaxLineBytes          int           = 1 << 20
	searchFileMaxMatchesPerFile  int           = 10
)
