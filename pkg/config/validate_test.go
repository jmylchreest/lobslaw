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
