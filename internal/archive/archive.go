// Package archive encodes portable knowledge snapshots, independent of storage
// encryption and Raft membership. It never executes or imports archive contents.
package archive

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
)

const (
	Format                = "lobslaw-knowledge"
	SchemaVersion         = 1
	MaxRecordBytes  int64 = 8 << 20
	MaxArchiveBytes int64 = 256 << 20
	MaxRecords            = 100000
	manifestName          = "manifest.json"
	privateMode           = 0o600
	ageHeaderPrefix       = "age-encryption.org/"
)

type Record struct {
	Kind string          `json:"kind"`
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data"`
}

type Manifest struct {
	Format        string            `json:"format"`
	SchemaVersion int               `json:"schema_version"`
	SnapshotID    string            `json:"snapshot_id"`
	CreatedAt     time.Time         `json:"created_at"`
	SourceVersion string            `json:"source_version"`
	Counts        map[string]int    `json:"counts"`
	Checksums     map[string]string `json:"checksums"`
	Omissions     []string          `json:"omissions"`
}

type Snapshot struct {
	Manifest Manifest
	Records  []Record
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func recordName(i int) string {
	return fmt.Sprintf("records/%06d.json", i)
}

func validateRecord(r Record) error {
	if r.Kind == "" || r.ID == "" || !json.Valid(r.Data) {
		return errors.New("record requires kind, id and valid JSON data")
	}
	return nil
}

// Write emits plaintext only when no recipients are supplied. The CLI requires
// an explicit --plaintext choice; backup callers should always supply recipients.
func Write(dst io.Writer, snapshot Snapshot, recipients ...age.Recipient) error {
	manifest, encoded, err := encodeSnapshot(snapshot)
	if err != nil {
		return err
	}

	var encrypted io.WriteCloser
	if len(recipients) > 0 {
		encrypted, err = age.Encrypt(dst, recipients...)
		if err != nil {
			return err
		}
		dst = encrypted
	}
	gz := gzip.NewWriter(dst)
	tw := tar.NewWriter(gz)
	write := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     privateMode,
			Size:     int64(len(data)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := write(manifestName, manifest); err != nil {
		return err
	}
	for i, data := range encoded {
		if err := write(recordName(i), data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if encrypted != nil {
		return encrypted.Close()
	}
	return nil
}

func encodeSnapshot(snapshot Snapshot) ([]byte, [][]byte, error) {
	if len(snapshot.Records) > MaxRecords {
		return nil, nil, errors.New("too many archive records")
	}
	if snapshot.Manifest.SnapshotID == "" || snapshot.Manifest.CreatedAt.IsZero() {
		return nil, nil, errors.New("snapshot id and creation time required")
	}
	records := slices.Clone(snapshot.Records)
	slices.SortFunc(records, func(a, b Record) int {
		if order := strings.Compare(a.Kind, b.Kind); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})

	m := snapshot.Manifest
	m.Format = Format
	m.SchemaVersion = SchemaVersion
	m.Counts = map[string]int{}
	m.Checksums = map[string]string{}
	encoded := make([][]byte, len(records))
	var total int64
	for i, r := range records {
		if err := validateRecord(r); err != nil {
			return nil, nil, err
		}
		if i > 0 && r.Kind == records[i-1].Kind && r.ID == records[i-1].ID {
			return nil, nil, errors.New("duplicate archive record")
		}
		data, err := json.Marshal(r)
		if err != nil {
			return nil, nil, err
		}
		total += int64(len(data))
		if int64(len(data)) > MaxRecordBytes || total > MaxArchiveBytes {
			return nil, nil, errors.New("archive size limit exceeded")
		}
		encoded[i] = data
		m.Counts[r.Kind]++
		m.Checksums[recordName(i)] = digest(data)
	}
	manifest, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	if int64(len(manifest)) > MaxRecordBytes || total+int64(len(manifest)) > MaxArchiveBytes {
		return nil, nil, errors.New("manifest size limit exceeded")
	}
	return manifest, encoded, nil
}

func decode(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

// Read authenticates the complete encrypted stream, gzip checksum and archive
// inventory before returning any records. It never extracts paths to disk.
func Read(src io.Reader, identities ...age.Identity) (Snapshot, error) {
	var out Snapshot
	br := bufio.NewReader(src)
	header, _ := br.Peek(len(ageHeaderPrefix))
	if bytes.HasPrefix(header, []byte(ageHeaderPrefix)) {
		r, err := age.Decrypt(br, identities...)
		if err != nil {
			return out, err
		}
		br = bufio.NewReader(r)
	}
	gz, err := gzip.NewReader(br)
	if err != nil {
		return out, err
	}
	defer func() { _ = gz.Close() }()
	gz.Multistream(false)
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil {
		return out, err
	}
	if h.Name != manifestName {
		return out, errors.New("manifest must be first")
	}
	data, err := readEntry(tr, h)
	if err != nil {
		return out, err
	}
	if err := decode(data, &out.Manifest); err != nil {
		return out, err
	}
	m := out.Manifest
	if err := validateManifest(m); err != nil {
		return out, err
	}

	counts := map[string]int{}
	seen := map[string]bool{}
	total := int64(len(data))
	for i := 0; ; i++ {
		h, err = tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
		if i >= MaxRecords || h.Name != recordName(i) {
			return out, errors.New("unexpected archive entry")
		}
		data, err = readEntry(tr, h)
		if err != nil {
			return out, err
		}
		total += int64(len(data))
		if total > MaxArchiveBytes {
			return out, errors.New("archive size limit exceeded")
		}
		if m.Checksums[h.Name] != digest(data) {
			return out, errors.New("archive checksum mismatch")
		}
		var r Record
		if err := decode(data, &r); err != nil {
			return out, err
		}
		if err := validateRecord(r); err != nil {
			return out, err
		}
		key := r.Kind + "\x00" + r.ID
		if seen[key] {
			return out, errors.New("duplicate archive record")
		}
		seen[key] = true
		counts[r.Kind]++
		out.Records = append(out.Records, r)
	}
	// Reading to EOF validates gzip's trailer and age's final authenticated chunk.
	var extra [1]byte
	if n, err := gz.Read(extra[:]); n != 0 || err != io.EOF {
		return out, errors.New("invalid archive trailer")
	}
	if _, err := br.Peek(1); err != io.EOF {
		return out, errors.New("trailing or unauthenticated archive data")
	}
	if len(out.Records) != len(m.Checksums) || len(counts) != len(m.Counts) {
		return out, errors.New("archive inventory mismatch")
	}
	for k, n := range counts {
		if m.Counts[k] != n {
			return out, errors.New("record count mismatch")
		}
	}
	return out, nil
}

func validateManifest(m Manifest) error {
	if m.Format != Format || m.SchemaVersion != SchemaVersion {
		return errors.New("unsupported archive format or schema")
	}
	if m.SnapshotID == "" || m.CreatedAt.IsZero() || m.Counts == nil || m.Checksums == nil {
		return errors.New("incomplete manifest")
	}
	if len(m.Checksums) > MaxRecords {
		return errors.New("too many records")
	}
	return nil
}

func readEntry(r io.Reader, h *tar.Header) ([]byte, error) {
	if h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > MaxRecordBytes {
		return nil, errors.New("invalid archive entry type or size")
	}
	return io.ReadAll(io.LimitReader(r, MaxRecordBytes+1))
}
