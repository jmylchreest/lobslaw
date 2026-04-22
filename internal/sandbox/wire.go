package sandbox

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// HelperSubcommand is the hidden `lobslaw <sub>` argument that
// dispatches to the reexec sandbox helper. Defined here (rather
// than in the main binary) so callers like Apply and the cmd-line
// dispatch share the same string.
const HelperSubcommand = "sandbox-exec"

// PolicyEnvVar is the env variable used to ferry a serialised Policy
// from the parent agent into the sandbox-exec helper child. Chosen
// (rather than argv) so the policy doesn't appear in `ps`.
const PolicyEnvVar = "LOBSLAW_SANDBOX_POLICY"

// EncodePolicy returns the base64(JSON) transport representation of
// p, suitable for assignment to $LOBSLAW_SANDBOX_POLICY. Portable
// across platforms; the decoding side handles non-Linux separately.
func EncodePolicy(p *Policy) (string, error) {
	if p == nil {
		return "", nil
	}
	blob, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(blob), nil
}

// DecodePolicy is the inverse of EncodePolicy. An empty input is
// treated as a zero Policy — helpful for the "dispatch-only" test
// mode where no enforcement is asked for.
func DecodePolicy(raw string) (*Policy, error) {
	if raw == "" {
		return &Policy{}, nil
	}
	blob, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("decode policy: %w", err)
	}
	var p Policy
	if err := json.Unmarshal(blob, &p); err != nil {
		return nil, fmt.Errorf("unmarshal policy: %w", err)
	}
	return &p, nil
}

