package local

import (
	"errors"

	"github.com/jmylchreest/lobslaw/internal/storage"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Factory is the storage.BackendFactory for the "local" type. Maps
// the replicated StorageMount proto (label + path) into a concrete
// Mount. Options are unused for this backend; extra keys are
// ignored rather than rejected so operators can share a single
// config shape across backends during migration.
func Factory(cfg *lobslawv1.StorageMount) (storage.Mount, error) {
	if cfg == nil {
		return nil, errors.New("local: nil mount config")
	}
	return New(Config{Label: cfg.Label, Source: cfg.Path})
}
