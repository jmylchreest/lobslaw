package memory

import (
	"errors"
	"fmt"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// ErrFSMUnsupported marks an entry this binary cannot safely interpret.
var ErrFSMUnsupported = errors.New("memory FSM cannot interpret committed entry")

type fsmFailure struct{ err error }

// Failed closes when application must stop. A nil FSM disables select cases.
func (f *FSM) Failed() <-chan struct{} {
	if f == nil {
		return nil
	}
	return f.failed
}

// Failure reports the first terminal apply error.
func (f *FSM) Failure() error {
	if f == nil {
		return nil
	}
	if failure := f.failure.Load(); failure != nil {
		return failure.err
	}
	return nil
}

// halt runs under f.mu. Never wait for Raft shutdown from its apply goroutine.
func (f *FSM) halt(index uint64, cause error) error {
	err := fmt.Errorf("%w at index %d: %w; upgrade or repair the node before restarting", ErrFSMUnsupported, index, cause)
	f.failure.Store(&fsmFailure{err: err})
	close(f.failed)
	return err
}

// Check support before mutation. Unknown optional protobuf fields on known
// payloads remain compatible; an unknown oneof variant decodes with no payload.
func validateLogEntrySupport(entry *lobslawv1.LogEntry) error {
	switch entry.Op {
	case lobslawv1.LogOp_LOG_OP_PUT:
		switch entry.Payload.(type) {
		case *lobslawv1.LogEntry_ArchiveBatch, *lobslawv1.LogEntry_SessionAppend:
			return nil
		}
	case lobslawv1.LogOp_LOG_OP_DELETE, lobslawv1.LogOp_LOG_OP_CLAIM:
	default:
		return fmt.Errorf("unknown log op: %v", entry.Op)
	}
	_, _, err := bucketAndPayload(entry)
	return err
}
