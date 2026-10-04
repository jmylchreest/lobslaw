package dataformat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const ManifestName = "data-migration.json"

const maxManifestBytes = 1 << 20

type Manifest struct {
	StateVersion int    `json:"state_version"`
	LogVersion   int    `json:"log_version"`
	Version      int    `json:"version"`
	Protocol     string `json:"protocol"`
	LegacyFormat string `json:"legacy_format,omitempty"`
	// Copied data is a recovery image, not permission to repeat external actions.
	RestoreRequired bool `json:"restore_required"`
}

func ReadManifest(dir string) (Manifest, error) {
	var m Manifest
	path := filepath.Join(dir, ManifestName)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if !info.Mode().IsRegular() {
		return m, errors.New("data manifest must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer func() { _ = file.Close() }()
	d := json.NewDecoder(io.LimitReader(file, maxManifestBytes))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, err
	}
	if d.Decode(new(any)) != io.EOF {
		return m, errors.New("trailing manifest data")
	}
	if m.StateVersion != StateVersion || m.LogVersion != LogVersion || m.Version != PhysicalVersion || m.Protocol != ClusterProtocol || !ValidLegacy(m.LegacyFormat) {
		return m, fmt.Errorf("unsupported physical data manifest (version %d, protocol %q)", m.Version, m.Protocol)
	}
	return m, nil
}
