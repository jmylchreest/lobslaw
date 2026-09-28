package node

import (
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// The gateway function normalises to compute, so the gate can no
// longer read it. If that rewrite and the gate ever disagree, a node
// declaring `gateway` boots with no channels and answers nothing —
// silently, because nothing else depends on the gateway existing.
func TestGatewayStillWiresAfterNormalisation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		fns     []types.NodeFunction
		enabled bool
		want    bool
	}{
		{
			name:    "legacy config naming the gateway function",
			fns:     []types.NodeFunction{types.FunctionCompute, types.FunctionGateway},
			enabled: true,
			want:    true,
		},
		{
			name:    "gateway function alone, which implies compute",
			fns:     []types.NodeFunction{types.FunctionGateway},
			enabled: true,
			want:    true,
		},
		{
			name:    "canonical config: compute plus the enable flag",
			fns:     []types.NodeFunction{types.FunctionCompute},
			enabled: true,
			want:    true,
		},
		{
			name:    "enabled but no agent to hand turns to",
			fns:     []types.NodeFunction{types.FunctionMemory},
			enabled: true,
			want:    false,
		},
		{
			name:    "compute present but channels switched off",
			fns:     []types.NodeFunction{types.FunctionCompute},
			enabled: false,
			want:    false,
		},
		{
			name:    "ui-web wires the HTTP surface without compute",
			fns:     []types.NodeFunction{types.FunctionUIWeb},
			enabled: false,
			want:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			normalised, _ := types.NormalizeFunctions(tc.fns)
			cfg := Config{
				Functions: normalised,
				Gateway:   config.GatewayConfig{Enabled: tc.enabled},
			}
			if got := gateGateway(cfg); got != tc.want {
				t.Errorf("gateGateway = %v, want %v (functions %v → %v, enabled=%v)",
					got, tc.want, tc.fns, normalised, tc.enabled)
			}
		})
	}
}

// memory and storage are mutually required, so every gate keyed on
// either must fire for a config naming just one. This used to be a
// validation error telling the operator to add the other.
func TestStatePairGatesTogether(t *testing.T) {
	t.Parallel()

	for _, in := range [][]types.NodeFunction{
		{types.FunctionMemory},
		{types.FunctionStorage},
		{types.FunctionPolicy},
	} {
		normalised, _ := types.NormalizeFunctions(in)
		cfg := Config{Functions: normalised}
		if !gateRaft(cfg) {
			t.Errorf("%v → %v: raft stages would not wire", in, normalised)
		}
		if !gateStorage(cfg) {
			t.Errorf("%v → %v: storage stage would not wire", in, normalised)
		}
	}

	// A compute-only node hosts neither.
	compute, _ := types.NormalizeFunctions([]types.NodeFunction{types.FunctionCompute})
	cfg := Config{Functions: compute}
	if gateRaft(cfg) || gateStorage(cfg) {
		t.Errorf("a compute-only node claimed state stages: %v", compute)
	}
}
