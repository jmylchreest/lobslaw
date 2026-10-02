package trace

// OTLP fixes these identity widths independently of the hash used to derive them.
const (
	traceIDBytes int = 16
	spanIDBytes  int = 8
)
