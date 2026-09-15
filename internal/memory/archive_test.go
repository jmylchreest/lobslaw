package memory

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestArchivePreservesContentButExcludesDerivedAndSecretState(t *testing.T) {
	s, path := newTestStore(t)
	put := func(bucket, key string, v proto.Message) {
		t.Helper()
		raw, err := proto.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Put(bucket, key, raw); err != nil {
			t.Fatal(err)
		}
	}
	put(BucketVectorRecords, "v", &lobslawv1.VectorRecord{Id: "v", Text: "a standalone summary", Owner: "user:alice", Embedding: []float32{1, 2}, EmbeddingModel: "old", Norm: 2})
	put(BucketScheduledTasks, "task", &lobslawv1.ScheduledTaskRecord{Id: "task", Schedule: "0 9 * * *", Enabled: true, ClaimedBy: "old-node"})
	put(BucketCommitments, "reminder", &lobslawv1.AgentCommitment{Id: "reminder", Reason: "remember", Status: "pending", ClaimedBy: "old-node"})
	if err := s.Put(BucketCredentials, "secret", []byte("not exported")); err != nil {
		t.Fatal(err)
	}
	key := s.key
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	records, err := ReadArchiveRecords(context.Background(), path, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d records", len(records))
	}
	for _, r := range records {
		if r.Kind == "documents" {
			v := &lobslawv1.VectorRecord{}
			if err := protojson.Unmarshal(r.Data, v); err != nil {
				t.Fatal(err)
			}
			if v.Text != "a standalone summary" || v.Owner != "user:alice" || len(v.Embedding) != 0 || v.EmbeddingModel != "" || v.Norm != 0 {
				t.Fatalf("bad document: %+v", v)
			}
		}
		if r.Kind == "scheduled-tasks" {
			v := &lobslawv1.ScheduledTaskRecord{}
			if err := protojson.Unmarshal(r.Data, v); err != nil {
				t.Fatal(err)
			}
			if v.Schedule != "0 9 * * *" || !v.Enabled || v.ClaimedBy != "" {
				t.Fatalf("bad schedule: %+v", v)
			}
		}
	}
	wrong, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadArchiveRecords(context.Background(), path, wrong); err == nil {
		t.Fatal("wrong key yielded successful backup")
	}
}

func TestArchiveReadOnlyAndSkillBytes(t *testing.T) {
	s, path := newTestStore(t)
	manifest := []byte("# signed bytes must survive\nname: example\n")
	sig := []byte{1, 2, 3}
	raw, err := proto.Marshal(&lobslawv1.SkillRecord{Name: "example", Version: "1.0.0", ManifestYaml: manifest, ManifestSig: sig})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(BucketSkills, "example@1.0.0", raw); err != nil {
		t.Fatal(err)
	}
	key := s.key
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	records, err := ReadArchiveRecords(context.Background(), path, key)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("export modified source database")
	}
	var skill lobslawv1.SkillRecord
	if len(records) != 1 {
		t.Fatalf("got %d records", len(records))
	}
	if err := protojson.Unmarshal(records[0].Data, &skill); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(skill.ManifestYaml, manifest) || !bytes.Equal(skill.ManifestSig, sig) {
		t.Fatal("signed bytes changed")
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := ReadArchiveRecords(context.Background(), missing, key); err == nil {
		t.Fatal("missing source accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("created missing source")
	}
}
