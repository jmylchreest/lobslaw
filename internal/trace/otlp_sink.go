package trace

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// OTLP export, written against the wire format rather than the SDK.
//
// The OpenTelemetry Go SDK brings a tracer provider, a span processor,
// a batcher and a context-propagation layer — every one of which this
// package already has its own version of. Adopting it would mean
// converting our spans into its spans so that its batcher could hand
// them to its exporter, which is a lot of machinery to serialise a
// struct we already own.
//
// So this is a Sink like any other: it takes the same Span, converts
// it, and posts it. The recorder's non-blocking dispatch and drop
// accounting apply unchanged, which is the property that matters — a
// collector that hangs must not reach the turn.

// DefaultOTLPFlush is how long a batch waits before going out.
//
// Short, because a trace an operator is watching for is one they are
// watching for now. The batch also flushes on size, so a busy node
// does not wait on the timer.
// defaultServiceName labels spans when the operator sets no service
// name. Matches the binary, so traces from an unconfigured node still
// group with the rest rather than arriving anonymous.
const defaultServiceName = "lobslaw"

const DefaultOTLPFlush = 2 * time.Second

// DefaultOTLPBatch is how many spans accumulate before a flush.
const DefaultOTLPBatch = 64

// DefaultOTLPTimeout bounds one export call.
//
// A collector that accepts a connection and then stops reading is the
// failure this exists for. Without a deadline the exporter blocks
// forever: the recorder's single background goroutine never returns,
// its queue fills, and every span is dropped for the life of the
// process — including the ones destined for the local file, which has
// nothing wrong with it. With a deadline the batch is lost and the
// next is tried.
const DefaultOTLPTimeout = 10 * time.Second

// OTLPConfig configures the exporter.
type OTLPConfig struct {
	// Endpoint is host:port of an OTLP/gRPC collector.
	Endpoint string
	// Insecure disables TLS. Named for what it does rather than
	// "secure = false", so a config file reads as an admission.
	Insecure bool
	// ServiceName identifies this node in the collector. Empty takes
	// "lobslaw".
	ServiceName string
	// NodeID distinguishes nodes in one deployment; traces are
	// per-node, and a collector receiving spans from three nodes with
	// no way to tell them apart is worse than three separate files.
	NodeID string

	BatchSize  int
	FlushEvery time.Duration
	// Timeout bounds one export call. Zero takes the default.
	Timeout      time.Duration
	DialOverride *grpc.ClientConn // tests inject a connection
}

// OTLPSink batches spans and exports them over OTLP/gRPC.
type OTLPSink struct {
	client   collectorpb.TraceServiceClient
	conn     *grpc.ClientConn
	ownsConn bool
	resource *resourcepb.Resource

	batchSize int
	timeout   time.Duration

	mu      sync.Mutex
	pending []*tracepb.Span
	closed  bool

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// NewOTLPSink dials the collector and starts the flush ticker.
//
// Dialling is lazy in gRPC — NewClient does not connect — which is the
// behaviour wanted here. A collector that is down at boot must not stop
// the node starting, and one that comes up later must start receiving
// without a restart.
func NewOTLPSink(cfg OTLPConfig) (*OTLPSink, error) {
	if cfg.Endpoint == "" && cfg.DialOverride == nil {
		return nil, fmt.Errorf("trace: otlp endpoint is required")
	}
	s := &OTLPSink{
		batchSize: cfg.BatchSize,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	if s.batchSize <= 0 {
		s.batchSize = DefaultOTLPBatch
	}
	s.timeout = cfg.Timeout
	if s.timeout <= 0 {
		s.timeout = DefaultOTLPTimeout
	}

	if cfg.DialOverride != nil {
		s.conn = cfg.DialOverride
	} else {
		creds := credentials.NewClientTLSFromCert(nil, "")
		if cfg.Insecure {
			creds = insecure.NewCredentials()
		}
		conn, err := grpc.NewClient(cfg.Endpoint, grpc.WithTransportCredentials(creds))
		if err != nil {
			return nil, fmt.Errorf("trace: otlp dial %q: %w", cfg.Endpoint, err)
		}
		s.conn, s.ownsConn = conn, true
	}
	s.client = collectorpb.NewTraceServiceClient(s.conn)
	s.resource = otlpResource(cfg)

	every := cfg.FlushEvery
	if every <= 0 {
		every = DefaultOTLPFlush
	}
	go s.flushLoop(every)
	return s, nil
}

func otlpResource(cfg OTLPConfig) *resourcepb.Resource {
	service := cfg.ServiceName
	if service == "" {
		service = defaultServiceName
	}
	attrs := []*commonpb.KeyValue{stringAttr("service.name", service)}
	if cfg.NodeID != "" {
		attrs = append(attrs, stringAttr("service.instance.id", cfg.NodeID))
	}
	return &resourcepb.Resource{Attributes: attrs}
}

// Write buffers a span, flushing when the batch is full.
//
// Called from the recorder's single background goroutine, so the only
// contention is with the flush ticker.
func (s *OTLPSink) Write(span Span) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("trace: otlp sink is closed")
	}
	s.pending = append(s.pending, toOTLP(span))
	full := len(s.pending) >= s.batchSize
	s.mu.Unlock()

	if full {
		return s.flush()
	}
	return nil
}

func (s *OTLPSink) flushLoop(every time.Duration) {
	defer close(s.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			// The error is the recorder's to count on the Write path;
			// a periodic flush has no caller to report to, and
			// retrying a batch a dead collector already refused would
			// grow the queue rather than drain it.
			_ = s.flush()
		case <-s.stop:
			_ = s.flush()
			return
		}
	}
}

func (s *OTLPSink) flush() error {
	s.mu.Lock()
	if len(s.pending) == 0 {
		s.mu.Unlock()
		return nil
	}
	batch := s.pending
	s.pending = nil
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	_, err := s.client.Export(ctx, &collectorpb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource:   s.resource,
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: batch}},
		}},
	})
	if err != nil {
		// The batch is DROPPED, not requeued. A collector that is down
		// stays down for minutes, and a queue that grows for the
		// duration is how a telemetry outage becomes a memory
		// incident. The recorder counts the failure.
		return fmt.Errorf("trace: otlp export: %w", err)
	}
	return nil
}

// Close flushes and releases the connection.
func (s *OTLPSink) Close() error {
	s.stopOnce.Do(func() {
		close(s.stop)
		<-s.done
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		if s.ownsConn && s.conn != nil {
			_ = s.conn.Close()
		}
	})
	return nil
}

// toOTLP converts one span to the wire format.
//
// The ids are HASHED rather than generated. OTLP wants 16 bytes of
// trace id and 8 of span id; our ids are ULIDs. Hashing is
// deterministic, so the same turn produces the same trace id on every
// node and in every export — which is what makes a retried batch
// idempotent and a turn's spans group in the collector rather than
// scattering across three traces.
func toOTLP(s Span) *tracepb.Span {
	start := uint64(s.StartedAt.UnixNano())
	end := start
	if s.Duration > 0 {
		end = start + uint64(s.Duration)
	}
	out := &tracepb.Span{
		TraceId:           traceID(s.TurnID),
		SpanId:            spanID(s.SpanID),
		Name:              otlpSpanName(s),
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: start,
		EndTimeUnixNano:   end,
		Attributes:        otlpAttrs(s),
	}
	if s.ParentID != "" {
		out.ParentSpanId = spanID(s.ParentID)
	}
	// A failed attempt is marked ERROR so a collector's own filters
	// find it. A SKIPPED candidate is not: it did not fail, it was
	// never tried, and colouring a protective decision red is how a
	// working trust floor gets reported as an outage.
	switch s.Outcome {
	case OutcomeAdvanced, OutcomeAborted:
		out.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: s.Error}
	case OutcomeOK:
		out.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK}
	case OutcomeSkipped:
		out.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_UNSET, Message: s.Error}
	}
	return out
}

// otlpSpanName is what shows in a waterfall. Kind plus provider, so a
// row is identifiable without opening it — a screen of spans all named
// "llm_call" is a screen with no information on it.
func otlpSpanName(s Span) string {
	if s.Provider == "" {
		return string(s.Kind)
	}
	return string(s.Kind) + " " + s.Provider
}

// otlpAttrs carries the numbers. NO CONTENT reaches here, and the
// conversion cannot introduce any: it reads named fields off a struct
// that has nowhere to put a prompt.
func otlpAttrs(s Span) []*commonpb.KeyValue {
	attrs := []*commonpb.KeyValue{
		stringAttr("lobslaw.turn_id", s.TurnID),
		stringAttr("lobslaw.kind", string(s.Kind)),
		stringAttr("lobslaw.outcome", string(s.Outcome)),
		intAttr("lobslaw.attempt", int64(s.Attempt)),
	}
	if s.Provider != "" {
		attrs = append(attrs, stringAttr("lobslaw.provider", s.Provider))
	}
	if s.Name != "" {
		attrs = append(attrs, stringAttr("lobslaw.model", s.Name))
	}
	if s.Usage.PromptTokens > 0 {
		attrs = append(attrs, intAttr("lobslaw.tokens.prompt", int64(s.Usage.PromptTokens)))
	}
	if s.Usage.CompletionTokens > 0 {
		attrs = append(attrs, intAttr("lobslaw.tokens.completion", int64(s.Usage.CompletionTokens)))
	}
	if s.Usage.CachedTokens > 0 {
		// Separate, because it is priced differently and folding it in
		// would overstate the cost of every cached turn.
		attrs = append(attrs, intAttr("lobslaw.tokens.cached", int64(s.Usage.CachedTokens)))
	}
	if s.CostUSD > 0 {
		attrs = append(attrs, floatAttr("lobslaw.cost_usd", s.CostUSD))
	}
	if s.ResultSize > 0 {
		attrs = append(attrs, intAttr("lobslaw.result_bytes", int64(s.ResultSize)))
	}
	if s.Unit != "" {
		// A non-token-billed call carries its own unit, so a zero token
		// count is not read as a free call.
		attrs = append(attrs, stringAttr("lobslaw.billing.unit", s.Unit),
			floatAttr("lobslaw.billing.quantity", s.Quantity))
	}
	return attrs
}

func stringAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func intAttr(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

func floatAttr(k string, v float64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v}}}
}

// traceID derives a stable 16-byte id from a turn id.
func traceID(turnID string) []byte {
	sum := sha256.Sum256([]byte("lobslaw-turn:" + turnID))
	return sum[:16]
}

// spanID derives a stable 8-byte id.
//
// A different prefix from traceID so a turn id and a span id that
// happened to be equal cannot collide into the same bytes — which
// would make a span its own parent, and is the kind of thing that
// shows up as one inexplicable trace six months later.
func spanID(id string) []byte {
	sum := sha256.Sum256([]byte("lobslaw-span:" + id))
	return sum[:8]
}
