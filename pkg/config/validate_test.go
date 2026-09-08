package config

import (
	"errors"
	"strings"
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

// The judge's prompt joins the vocabulary with ", ", so a comma inside
// a declared tag reads back as two tags the operator never wrote, here
// "a" and "b" instead of "a,b", and the chain ends up routing on a tag
// nobody can name.
func TestValidateRejectsACommaInATriggerDomain(t *testing.T) {
	t.Parallel()
	c := &Config{Compute: ComputeConfig{Chains: []ChainConfig{{
		Label:   "finance",
		Trigger: ChainTriggerConfig{Domains: []string{"a,b", "legal"}},
	}}}}
	err := c.Validate()
	if err == nil {
		t.Fatal("a comma in trigger.domains was accepted")
	}
	if !errors.Is(err, types.ErrInvalidConfig) {
		t.Errorf("err = %v, want wraps ErrInvalidConfig", err)
	}
}

// A newline in a declared tag lands verbatim inside the judge's
// single-line system prompt instead of staying a tag.
func TestValidateRejectsANewlineInATriggerDomain(t *testing.T) {
	t.Parallel()
	c := &Config{Compute: ComputeConfig{Chains: []ChainConfig{{
		Label:   "finance",
		Trigger: ChainTriggerConfig{Domains: []string{"legal\ninstructions"}},
	}}}}
	err := c.Validate()
	if err == nil {
		t.Fatal("a newline in trigger.domains was accepted")
	}
	if !errors.Is(err, types.ErrInvalidConfig) {
		t.Errorf("err = %v, want wraps ErrInvalidConfig", err)
	}
}

// A tab or carriage return is the same shape of problem as a newline,
// so it is rejected the same way.
func TestValidateRejectsATabInATriggerDomain(t *testing.T) {
	t.Parallel()
	c := &Config{Compute: ComputeConfig{Chains: []ChainConfig{{
		Label:   "finance",
		Trigger: ChainTriggerConfig{Domains: []string{"legal\ttag"}},
	}}}}
	err := c.Validate()
	if err == nil {
		t.Fatal("a tab in trigger.domains was accepted")
	}
	if !errors.Is(err, types.ErrInvalidConfig) {
		t.Errorf("err = %v, want wraps ErrInvalidConfig", err)
	}
}

// A trigger.domains entry long enough to need a bound is not a tag; it
// is a paragraph pasted into the wrong field.
func TestValidateRejectsAnOverlongTriggerDomain(t *testing.T) {
	t.Parallel()
	c := &Config{Compute: ComputeConfig{Chains: []ChainConfig{{
		Label:   "finance",
		Trigger: ChainTriggerConfig{Domains: []string{strings.Repeat("a", domainTagMaxLen+1)}},
	}}}}
	err := c.Validate()
	if err == nil {
		t.Fatal("an overlong trigger.domains entry was accepted")
	}
	if !errors.Is(err, types.ErrInvalidConfig) {
		t.Errorf("err = %v, want wraps ErrInvalidConfig", err)
	}
}

// The bound is a length no real tag needs, not an off-by-one trap: a
// tag exactly at it is still accepted.
func TestValidateAcceptsATriggerDomainAtTheLengthBound(t *testing.T) {
	t.Parallel()
	c := &Config{Compute: ComputeConfig{Chains: []ChainConfig{{
		Label:   "finance",
		Trigger: ChainTriggerConfig{Domains: []string{strings.Repeat("a", domainTagMaxLen)}},
	}}}}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}
