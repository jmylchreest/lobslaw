package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"testing"
	"time"

	"filippo.io/age"
)

func fixture() Snapshot {
	return Snapshot{
		Manifest: Manifest{
			SnapshotID:    "test-snapshot",
			CreatedAt:     time.Now().UTC(),
			SourceVersion: "test",
		},
		Records: []Record{
			{
				Kind: "documents",
				ID:   "memory-1",
				Data: json.RawMessage(`{"text":"remember this","owner":"user:alice"}`),
			},
			{
				Kind: "scheduled-tasks",
				ID:   "task-1",
				Data: json.RawMessage(`{"schedule":"0 9 * * *","enabled":true}`),
			},
		},
	}
}

func rewriteArchive(t *testing.T, change func(*tar.Header, []byte) []byte) []byte {
	t.Helper()
	var source, out bytes.Buffer
	if err := Write(&source, fixture()); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	zipped := gzip.NewWriter(&out)
	tw := tar.NewWriter(zipped)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		data = change(h, data)
		h.Size = int64(len(data))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestArchiveRejectsInvalidSchemaInventoryAndPaths(t *testing.T) {
	changes := map[string]func(*tar.Header, []byte) []byte{
		"schema": func(h *tar.Header, b []byte) []byte {
			if h.Name == manifestName {
				return bytes.Replace(b, []byte(`"schema_version":1`), []byte(`"schema_version":999`), 1)
			}
			return b
		},
		"checksum": func(h *tar.Header, b []byte) []byte {
			if h.Name != manifestName {
				return bytes.ReplaceAll(b, []byte("remember"), []byte("modified"))
			}
			return b
		},
		"traversal": func(h *tar.Header, b []byte) []byte {
			if h.Name != manifestName {
				h.Name = "../escape"
			}
			return b
		},
		"symlink": func(h *tar.Header, b []byte) []byte {
			if h.Name != manifestName {
				h.Typeflag = tar.TypeSymlink
				h.Linkname = "/etc/passwd"
				return nil
			}
			return b
		},
		"counts": func(h *tar.Header, b []byte) []byte {
			if h.Name == manifestName {
				return bytes.Replace(b, []byte(`"documents":1`), []byte(`"documents":2`), 1)
			}
			return b
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			if _, err := Read(bytes.NewReader(rewriteArchive(t, change))); err == nil {
				t.Fatal("accepted invalid archive")
			}
		})
	}
}

func TestEncryptedRoundTrip(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Write(&out, fixture(), id.Recipient()); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte("remember this")) {
		t.Fatal("plaintext leaked")
	}
	got, err := Read(bytes.NewReader(out.Bytes()), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 2 || got.Manifest.Counts["scheduled-tasks"] != 1 {
		t.Fatalf("unexpected inventory: %+v", got.Manifest)
	}
	if string(got.Records[0].Data) != string(fixture().Records[0].Data) {
		t.Fatal("record content changed")
	}
	if _, err := Read(bytes.NewReader(out.Bytes())); err == nil {
		t.Fatal("encrypted archive accepted without identity")
	}
	for _, n := range []int{1, out.Len() / 2, out.Len() - 1} {
		if _, err := Read(bytes.NewReader(out.Bytes()[:n]), id); err == nil {
			t.Fatalf("accepted truncation at %d", n)
		}
	}
	damaged := bytes.Clone(out.Bytes())
	damaged[len(damaged)-8] ^= 1
	if _, err := Read(bytes.NewReader(damaged), id); err == nil {
		t.Fatal("accepted tampering")
	}
}

func TestArchiveRejectsDuplicateRecordsAndInvalidJSON(t *testing.T) {
	s := fixture()
	s.Records = append(s.Records, s.Records[0])
	if err := Write(&bytes.Buffer{}, s); err == nil {
		t.Fatal("duplicate record accepted")
	}
	s = fixture()
	s.Records[0].Data = json.RawMessage(`broken`)
	if err := Write(&bytes.Buffer{}, s); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestPlaintextArchiveRoundTripAndTrailingData(t *testing.T) {
	var out bytes.Buffer
	if err := Write(&out, fixture()); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(bytes.NewReader(out.Bytes())); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(bytes.NewReader(append(out.Bytes(), 'x'))); err == nil {
		t.Fatal("trailing data accepted")
	}
}
