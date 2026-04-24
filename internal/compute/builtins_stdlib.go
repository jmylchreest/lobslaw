package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// RegisterStdlibBuiltins installs the Go-native tools every node
// ships with. Caller (node.New) also Register()s each ToolDef from
// StdlibToolDefs into the exec Registry so the LLM sees them in
// its function-calling list.
func RegisterStdlibBuiltins(b *Builtins) error {
	if err := b.Register("current_time", currentTimeBuiltin); err != nil {
		return err
	}
	if err := b.Register("read_file", readFileBuiltin); err != nil {
		return err
	}
	return b.Register("search_files", searchFilesBuiltin)
}

func StdlibToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "current_time",
			Path:        BuiltinScheme + "current_time",
			Description: "Returns the current wall-clock time as JSON with fields utc (RFC3339), local (RFC3339), zone (IANA name), offset_secs, and unix (epoch seconds). Pass optional zones=[\"America/Los_Angeles\",\"Asia/Tokyo\"] to additionally return the time in each IANA timezone under the zones field. Call this when the user asks about the time or date; do not guess.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"zones": {
						"type": "array",
						"items": {"type": "string"},
						"description": "Optional list of IANA timezone names (e.g. America/Los_Angeles, Europe/London, UTC) to additionally return times for."
					}
				},
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "read_file",
			Path:        BuiltinScheme + "read_file",
			Description: "Read the contents of a text file. Pass path (absolute). Optional offset (0-indexed line number to start at) and limit (max lines to return, default 200). Returns JSON with path, line_count, and content. Use this to inspect files the user references rather than guessing their contents.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Absolute filesystem path."},
					"offset": {"type": "integer", "description": "0-indexed line number to start at."},
					"limit": {"type": "integer", "description": "Max lines to return (default 200, cap 2000)."}
				},
				"required": ["path"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "search_files",
			Path:        BuiltinScheme + "search_files",
			Description: "Search for a pattern across text files. Pass pattern (regex) and optional path (default cwd) + glob (e.g. \"*.md\"). Returns JSON with matches [{path, line_number, line}]. Uses ripgrep when available, grep -rn otherwise. Capped at 100 matches.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"pattern": {"type": "string", "description": "Search pattern (regex)."},
					"path": {"type": "string", "description": "Directory or file to search under. Default current working directory."},
					"glob": {"type": "string", "description": "Filename glob filter (e.g. *.go)."}
				},
				"required": ["pattern"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
	}
}

// currentTimeBuiltin returns the current wall-clock time in UTC
// and the host's local timezone. When args["zones"] is set (JSON-
// encoded string array from the LLM's tool-call), the response
// also includes per-zone times. JSON output so the LLM can parse
// structured fields instead of regex-splitting.
func currentTimeBuiltin(_ context.Context, args map[string]string) ([]byte, int, error) {
	now := time.Now()
	zoneName, offsetSec := now.Zone()
	payload := map[string]any{
		"utc":         now.UTC().Format(time.RFC3339Nano),
		"local":       now.Format(time.RFC3339Nano),
		"zone":        zoneName,
		"offset_secs": offsetSec,
		"unix":        now.Unix(),
	}

	// The LLM's function-call arguments arrive as JSON; the
	// executor flattens scalar fields into args directly, but
	// arrays arrive as their JSON-encoded string form. Parse
	// explicitly so a malformed value is a tool-level error rather
	// than a silent no-op.
	if raw, ok := args["zones"]; ok && raw != "" {
		var zones []string
		if err := json.Unmarshal([]byte(raw), &zones); err != nil {
			return nil, 2, fmt.Errorf("zones must be a JSON array of IANA zone names: %w", err)
		}
		zoneTimes := make(map[string]any, len(zones))
		for _, z := range zones {
			loc, err := time.LoadLocation(z)
			if err != nil {
				zoneTimes[z] = map[string]any{"error": "unknown IANA zone: " + err.Error()}
				continue
			}
			inZone := now.In(loc)
			_, off := inZone.Zone()
			zoneTimes[z] = map[string]any{
				"time":        inZone.Format(time.RFC3339Nano),
				"offset_secs": off,
			}
		}
		payload["zones"] = zoneTimes
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return nil, 1, err
	}
	return out, 0, nil
}
