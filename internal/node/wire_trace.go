package node

import (
	"fmt"
	"path/filepath"

	"github.com/jmylchreest/lobslaw/internal/trace"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Turn tracing.
//
// Off unless asked for, and off means ABSENT: with tracing disabled
// n.traces stays nil, and a nil *trace.Recorder is usable — every
// method tolerates it. So the instrumented code paths call
// rec.Record(...) unconditionally rather than branching on whether
// tracing exists, and a deployment that never enabled it carries no
// runtime check at all.

// traceDirName sits under DataDir alongside the skill cache, not in a
// storage mount. A mount is shared and durable; this is neither. It is
// per-node telemetry that can be deleted at any time, and putting it
// somewhere an operator might back up would invite restoring stale
// traces over a node that had moved on.
const traceDirName = "traces"

// traceReadDir resolves where this node's traces live, or "" when
// there is nowhere.
//
// Shared by the recorder and the read service, so the thing that
// writes traces and the thing that serves them cannot disagree about
// which directory that is.
func (n *Node) traceReadDir() string {
	if dir := n.cfg.Trace.Dir; dir != "" {
		return dir
	}
	if n.cfg.DataDir == "" {
		return ""
	}
	return filepath.Join(n.cfg.DataDir, traceDirName)
}

// startTracing builds the recorder. Returns nil having built nothing
// when tracing is off.
func (n *Node) startTracing() error {
	if !n.cfg.Trace.Enabled {
		return nil
	}
	dir := n.traceReadDir()
	if dir == "" {
		{
			// Not an error. A node with no data dir has nowhere
			// per-node to put anything, and refusing to boot over
			// telemetry would be the wrong trade. Said out loud,
			// because "I turned tracing on and got nothing" is
			// otherwise indistinguishable from a quiet deployment.
			n.log.Warn("trace: enabled but there is no data dir and no trace.dir; nothing will be recorded")
			return nil
		}
	}

	// Constructed eagerly so a directory that cannot be written fails
	// HERE, while an operator is looking at a boot error, rather than
	// silently dropping every span for the life of the process.
	sink, err := trace.NewFileSink(dir, n.cfg.Trace.MaxBytes)
	if err != nil {
		return fmt.Errorf("trace: %w", err)
	}
	sinks := []trace.Sink{sink}

	// In ADDITION to the file, not instead of it. The file is the
	// record; the collector is where you look. A collector going down
	// must not lose the trace of the turn that was failing while it
	// was down — which is exactly the trace anybody would want
	// afterwards.
	if endpoint := n.cfg.Trace.OTLPEndpoint; endpoint != "" {
		otlp, err := trace.NewOTLPSink(trace.OTLPConfig{
			Endpoint:    endpoint,
			Insecure:    n.cfg.Trace.OTLPInsecure,
			ServiceName: n.cfg.Trace.ServiceName,
			NodeID:      n.cfg.NodeID,
		})
		if err != nil {
			// Not fatal, and the file sink is why. Losing the collector
			// degrades tracing to local-only; refusing to boot over it
			// would take the assistant down to protect telemetry.
			n.log.Error("trace: otlp export disabled", "endpoint", endpoint, "err", err)
		} else {
			sinks = append(sinks, otlp)
			n.log.Info("trace: exporting to a collector",
				"endpoint", endpoint, "insecure", n.cfg.Trace.OTLPInsecure)
		}
	}

	n.traces = trace.NewRecorder(n.log, sinks...)
	n.log.Info("trace: recording turns", "dir", dir,
		"max_bytes", n.cfg.Trace.MaxBytes, "sinks", len(sinks), "content_recorded", false)
	return nil
}

// wireTraceService registers TraceService, so an operator can ask this
// specific node what it recorded rather than reading whatever
// directory happens to be on their laptop.
//
// Registered even when tracing is OFF. The service reports enabled=false
// and an empty listing, which is a different answer from "no such
// service" and from "this node served no turns" — and only one of the
// three is fixed by editing config.
func (n *Node) wireTraceService() error {
	lobslawv1.RegisterTraceServiceServer(n.server,
		trace.NewService(n.cfg.NodeID, n.traceReadDir(), n.cfg.Trace.Enabled))
	return nil
}

// stopTracing drains and closes. Safe when tracing was never on.
func (n *Node) stopTracing() {
	if n.traces == nil {
		return
	}
	_ = n.traces.Close()
}
