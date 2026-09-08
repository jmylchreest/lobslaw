package config

import (
	"errors"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestValidateRejectsMemoryWithoutStorage(t *testing.T) {
	t.Parallel()
	c := &Config{
		Memory: MemoryConfig{
			Enabled:    true,
			Encryption: EncryptionConfig{KeyRef: "env:FOO"},
		},
		Storage: StorageConfig{Enabled: false},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, types.ErrInvalidConfig) {
		t.Errorf("err = %v, want wraps ErrInvalidConfig", err)
	}
}

func TestValidateAcceptsMemoryWithStorage(t *testing.T) {
	t.Parallel()
	c := &Config{
		Memory: MemoryConfig{
			Enabled:    true,
			Encryption: EncryptionConfig{KeyRef: "env:FOO"},
		},
		Storage: StorageConfig{Enabled: true},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidateMemoryDisabledIgnoresStorage(t *testing.T) {
	t.Parallel()
	c := &Config{
		Memory:  MemoryConfig{Enabled: false},
		Storage: StorageConfig{Enabled: false},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("memory disabled should pass without storage: %v", err)
	}
}

// A domain is a tag the preflight is offered and a chain routes on. An
// empty one is offered to nobody, so the rule that survives is narrower
// than the one written: here, "hard finance turns" becomes every hard
// turn.
func TestValidateRejectsAnEmptyTriggerDomain(t *testing.T) {
	t.Parallel()
	c := &Config{Compute: ComputeConfig{Chains: []ChainConfig{{
		Label:   "finance",
		Trigger: ChainTriggerConfig{MinComplexity: 60, Domains: []string{"finance", "  "}},
	}}}}
	err := c.Validate()
	if err == nil {
		t.Fatal("an empty trigger.domains entry was accepted")
	}
	if !errors.Is(err, types.ErrInvalidConfig) {
		t.Errorf("err = %v, want wraps ErrInvalidConfig", err)
	}
}

// Both sides of the match normalise, so the spelling an operator
// happens to use is not a boot failure.
func TestValidateAcceptsATriggerDomainAsWritten(t *testing.T) {
	t.Parallel()
	c := &Config{Compute: ComputeConfig{Chains: []ChainConfig{{
		Label:   "private-tier",
		Trigger: ChainTriggerConfig{Domains: []string{"Legal", " medical "}},
	}}}}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}
