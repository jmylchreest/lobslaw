package memory

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/archive"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func archiveTestRecord(t *testing.T, kind, id string, msg proto.Message) archive.Record {
	t.Helper()
	data, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return archive.Record{Kind: kind, ID: id, Data: data}
}

func TestArchivePlanMapsOwnersAndFindsConflicts(t *testing.T) {
	source := []archive.Record{
		archiveTestRecord(t, "documents", "summary", &lobslawv1.VectorRecord{
			Id: "summary", Text: "unchanged Dream summary", Owner: "user:alice",
			SourceIds: []string{"retired-episode"},
		}),
	}
	if _, err := PlanArchiveImport(nil, source, ArchiveImportOptions{}); err == nil {
		t.Fatal("accepted an owner without an explicit identity mapping")
	}
	opts := ArchiveImportOptions{Owners: map[string]string{"user:alice": "user:bob"}}
	plan, err := PlanArchiveImport(nil, source, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Additions) != 1 || plan.Embeddings != 1 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	var rec lobslawv1.VectorRecord
	if err := protojson.Unmarshal(plan.Additions[0].Data, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Owner != "user:bob" || rec.Text != "unchanged Dream summary" || rec.SourceIds[0] != "retired-episode" {
		t.Fatal("mapping changed content or provenance")
	}
	duplicate, err := PlanArchiveImport(plan.Additions, source, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(duplicate.Additions) != 0 || len(duplicate.Duplicates) != 1 {
		t.Fatalf("repeat was not a no-op: %+v", duplicate)
	}
	rec.Text = "destination edit"
	destination := []archive.Record{archiveTestRecord(t, "documents", "summary", &rec)}
	conflict, err := PlanArchiveImport(destination, source, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflict.Conflicts) != 1 || len(conflict.Additions) != 0 {
		t.Fatalf("conflict was not reported: %+v", conflict)
	}
}

func TestArchivePlanPausesExecutableState(t *testing.T) {
	manifest := []byte("# preserve these signed bytes\nname: example\n")
	sig := []byte{1, 2, 3}
	source := []archive.Record{
		archiveTestRecord(t, "skills", "example@1", &lobslawv1.SkillRecord{
			Name: "example", Version: "1", ManifestYaml: manifest, ManifestSig: sig,
			Active: true, Tier: lobslawv1.SkillTier_SKILL_TIER_SIGNED,
		}),
		archiveTestRecord(t, "scheduled-tasks", "task", &lobslawv1.ScheduledTaskRecord{
			Id: "task", Owner: "user:alice", Schedule: "0 9 * * *", Enabled: true,
		}),
		archiveTestRecord(t, "commitments", "reminder", &lobslawv1.AgentCommitment{
			Id: "reminder", Owner: "user:alice", Status: "pending", Reason: "remember",
		}),
	}
	opts := ArchiveImportOptions{
		Owners:         map[string]string{"user:alice": "user:alice"},
		SourceTimezone: "Europe/London",
	}
	plan, err := PlanArchiveImport(nil, source, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Paused) != 3 {
		t.Fatalf("expected three paused records: %+v", plan)
	}
	for _, record := range plan.Additions {
		switch record.Kind {
		case "skills":
			var skill lobslawv1.SkillRecord
			if err := protojson.Unmarshal(record.Data, &skill); err != nil {
				t.Fatal(err)
			}
			if skill.Active || !bytes.Equal(skill.ManifestYaml, manifest) || !bytes.Equal(skill.ManifestSig, sig) {
				t.Fatal("skill activated or signed bytes changed")
			}
		case "scheduled-tasks":
			var task lobslawv1.ScheduledTaskRecord
			if err := protojson.Unmarshal(record.Data, &task); err != nil {
				t.Fatal(err)
			}
			if task.Enabled || task.Schedule != "CRON_TZ=Europe/London 0 9 * * *" {
				t.Fatal("task activated or lost its source timezone")
			}
		case "commitments":
			var reminder lobslawv1.AgentCommitment
			if err := protojson.Unmarshal(record.Data, &reminder); err != nil {
				t.Fatal(err)
			}
			if reminder.Status != "paused" || reminder.Reason != "remember" {
				t.Fatal("pending commitment was not paused")
			}
		}
	}
}

func TestArchivePlanRejectsInvalidRecordsBeforeApply(t *testing.T) {
	tests := []struct {
		name   string
		record archive.Record
	}{
		{"unknown kind", archive.Record{Kind: "credentials", ID: "secret", Data: []byte(`{}`)}},
		{"unknown field", archive.Record{Kind: "documents", ID: "v", Data: []byte(`{"id":"v","future_field":true}`)}},
		{"wrong id", archiveTestRecord(t, "documents", "v", &lobslawv1.VectorRecord{Id: "different"})},
		{"missing session", archiveTestRecord(t, "session-messages", sessionMessageKey("rest:1", 1), &lobslawv1.SessionMessage{SessionId: "rest:1", Seq: 1})},
		{"bad blob digest", archiveTestRecord(t, "skill-blobs", "sha256:bad", &lobslawv1.SkillBlob{Digest: "sha256:bad", Content: []byte("payload")})},
		{"missing blob", archiveTestRecord(t, "skills", "example@1", &lobslawv1.SkillRecord{Name: "example", Version: "1", Files: map[string]string{"handler.py": "sha256:missing"}})},
		{"learned traversal", archiveTestRecord(t, "learned", "skill:bad", &lobslawv1.SelfTaughtRecord{
			Id: "skill:bad", Files: map[string]string{"../outside": "payload"},
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := PlanArchiveImport(nil, []archive.Record{tt.record}, ArchiveImportOptions{}); err == nil {
				t.Fatal("accepted invalid archive")
			}
		})
	}
}

func TestArchivePlanReportsConflictingSessionBeforeTranscriptDependencies(t *testing.T) {
	existing := []archive.Record{
		archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", FirstSeq: 1, NextSeq: 2}),
	}
	incoming := []archive.Record{
		archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", FirstSeq: 10, NextSeq: 12}),
		archiveTestRecord(t, "session-messages", sessionMessageKey("chat", 10), &lobslawv1.SessionMessage{SessionId: "chat", Seq: 10}),
	}
	plan, err := PlanArchiveImport(existing, incoming, ArchiveImportOptions{})
	if err != nil {
		t.Fatalf("conflict preview failed: %v", err)
	}
	if len(plan.Conflicts) != 1 || plan.Conflicts[0].Kind != "sessions" {
		t.Fatalf("session conflict missing: %+v", plan)
	}
	// Keeping the destination index cannot make the source transcript valid.
	if _, err := PlanArchiveImport(existing, incoming, ArchiveImportOptions{KeepExisting: true}); err == nil {
		t.Fatal("accepted messages outside the retained session range")
	}
}
