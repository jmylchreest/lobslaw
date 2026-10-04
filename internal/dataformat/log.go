package dataformat

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
)

// LogVersionField is an additive field in LogEntry, not an application payload.
const LogVersionField protowire.Number = 1000

var ErrAmbiguous = errors.New("unversioned Raft payload is ambiguous; select a verified legacy format with data migrate")

func ValidLegacy(profile string) bool {
	return profile == "" || profile == LegacyMain || profile == LegacyTeams || profile == LegacyTeamsEarly
}

// NormalizeLog translates the outer envelope only. Nested payload bytes, CAS
// revisions, unknown optional fields, IDs and Raft terms/indexes remain intact.
// It operates on a copy; the committed source log is never rewritten.
func NormalizeLog(raw []byte, profile string) ([]byte, error) {
	if !ValidLegacy(profile) {
		return nil, fmt.Errorf("unsupported legacy format %q", profile)
	}
	version, err := logVersion(raw)
	if err != nil {
		return nil, err
	}
	if version != 0 && version != LogVersion {
		return nil, fmt.Errorf("unsupported Raft log version %d", version)
	}
	if version == LogVersion {
		return append([]byte(nil), raw...), nil
	}
	out := make([]byte, 0, len(raw)+8)
	for len(raw) > 0 {
		number, typ, n := protowire.ConsumeTag(raw)
		size := protowire.ConsumeFieldValue(number, typ, raw[n:])
		// logVersion already validated the framing.
		target := number
		switch number {
		case 37:
			if profile == "" {
				return nil, ErrAmbiguous
			}
			if profile == LegacyTeamsEarly {
				target = 51
			}
		case 38, 39:
			if profile == "" {
				return nil, ErrAmbiguous
			}
			if profile == LegacyTeams || profile == LegacyTeamsEarly {
				target = number + 15
			}
		}
		if target != number && typ != protowire.BytesType {
			return nil, errors.New("invalid legacy payload wire type")
		}
		out = protowire.AppendTag(out, target, typ)
		out = append(out, raw[n:n+size]...)
		raw = raw[n+size:]
	}
	out = protowire.AppendTag(out, LogVersionField, protowire.VarintType)
	return protowire.AppendVarint(out, LogVersion), nil
}

func logVersion(raw []byte) (uint64, error) {
	var version uint64
	seen := false
	for len(raw) > 0 {
		number, typ, n := protowire.ConsumeTag(raw)
		if n < 0 {
			return 0, protowire.ParseError(n)
		}
		size := protowire.ConsumeFieldValue(number, typ, raw[n:])
		if size < 0 {
			return 0, protowire.ParseError(size)
		}
		if number == LogVersionField {
			if seen || typ != protowire.VarintType {
				return 0, errors.New("invalid or duplicate log version")
			}
			seen = true
			version, _ = protowire.ConsumeVarint(raw[n:])
			if version == 0 {
				return 0, errors.New("explicit log version zero is invalid")
			}
		}
		raw = raw[n+size:]
	}
	return version, nil
}

// CurrentLog is used by the proposal boundary: application messages already use
// current field numbers. Legacy interpretation is confined to historical reads.
func CurrentLog(raw []byte) ([]byte, error) { return NormalizeLog(raw, LegacyMain) }
