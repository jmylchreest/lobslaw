package minimax

// Provider error codes distinguish retryable throttling and internal failures.
const (
	statusRateLimited        int = 1002
	statusConcurrencyLimited int = 1039
	statusUnknownError       int = 1000
	statusInternalError      int = 1013
)
