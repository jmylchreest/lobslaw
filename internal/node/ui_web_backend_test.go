package node

import (
	"errors"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestUIWebWithoutComputeRequiresBackend(t *testing.T) {
	t.Parallel()
	err := validateUIWebBackend(Config{
		Functions: []types.NodeFunction{types.FunctionUIWeb},
	})
	if !errors.Is(err, ErrUIWebBackendRequired) {
		t.Fatalf("got %v, want ErrUIWebBackendRequired", err)
	}
}

func TestUIWebWithComputeDoesNotRequireBackend(t *testing.T) {
	t.Parallel()
	err := validateUIWebBackend(Config{
		Functions: []types.NodeFunction{types.FunctionUIWeb, types.FunctionCompute},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUIWebWithBackendPasses(t *testing.T) {
	t.Parallel()
	err := validateUIWebBackend(Config{
		Functions: []types.NodeFunction{types.FunctionUIWeb},
		UIWeb:     config.UIWebConfig{Backend: "compute-1:7443"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUIWebOffIgnoresBackend(t *testing.T) {
	t.Parallel()
	err := validateUIWebBackend(Config{
		Functions: []types.NodeFunction{types.FunctionMemory},
	})
	if err != nil {
		t.Fatal(err)
	}
}
