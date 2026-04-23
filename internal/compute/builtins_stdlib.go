package compute

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// RegisterStdlibBuiltins installs the Go-native tools every node
// ships with. Caller (node.New) also Register()s each ToolDef from
// StdlibToolDefs into the exec Registry so the LLM sees them in
// its function-calling list.
func RegisterStdlibBuiltins(b *Builtins) error {
	return b.Register("current_time", currentTimeBuiltin)
}

func StdlibToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:             "current_time",
			Path:             BuiltinScheme + "current_time",
			Description:      "Returns the current wall-clock time as JSON with fields utc (RFC3339), local (RFC3339), zone (IANA name), offset_secs, and unix (epoch seconds). Call this when the user asks about the time or date; do not guess.",
			ParametersSchema: []byte(`{"type":"object","properties":{},"additionalProperties":false}`),
			RiskTier:         types.RiskReversible,
		},
	}
}

// currentTimeBuiltin returns the current wall-clock time in UTC
// and the host's local timezone. JSON output so the LLM can parse
// structured fields instead of regex-splitting.
func currentTimeBuiltin(_ context.Context, _ map[string]string) ([]byte, int, error) {
	now := time.Now()
	zoneName, offsetSec := now.Zone()
	payload := map[string]any{
		"utc":         now.UTC().Format(time.RFC3339Nano),
		"local":       now.Format(time.RFC3339Nano),
		"zone":        zoneName,
		"offset_secs": offsetSec,
		"unix":        now.Unix(),
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, 1, err
	}
	return out, 0, nil
}
