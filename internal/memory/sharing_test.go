package memory

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/sharing"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func sharingFixture(t *testing.T) sharing.Artifact {
	t.Helper()
	a, err := sharing.Build(sharing.Package{Format: sharing.Format, Schema: 1, Name: "weather", Version: "0.0.0", Manifest: []byte("name: weather\nversion: 0.0.0\nruntime: prose\nbody: SKILL.md\n"), Files: map[string][]byte{"SKILL.md": []byte("Weather instructions")}, Schedules: []sharing.Schedule{{Key: "daily", Name: "Daily weather", Cron: "0 9 * * *", Timezone: "Europe/London", Prompt: "weather for {{city}}", NotifyOn: "never"}}, Inputs: []string{"city"}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSharingInstallActivateAndRetry(t *testing.T) {
	node, fsm := newTestRaft(t)
	s := NewSharingStore(node, fsm.Store())
	ctx := context.Background()
	a := sharingFixture(t)
	p, err := s.PlanInstall(a, "user:alice", map[string]string{"city": "London"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsm.Store().Get(BucketSkills, "weather@0.0.0"); err == nil {
		t.Fatal("preview wrote a skill")
	}
	if err := s.Apply(ctx, p, p.Digest); err != nil {
		t.Fatal(err)
	}
	sk, _ := s.skill("weather", "0.0.0")
	if sk.Active {
		t.Fatal("import activated skill")
	}
	job, err := s.task(p.ScheduleIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if job.Enabled || job.Owner != "user:alice" || job.Params["prompt"] != "weather for London" {
		t.Fatalf("bad staged job: %+v", job)
	}
	retry, err := s.PlanInstall(a, "user:alice", map[string]string{"city": "London"})
	if err != nil {
		t.Fatal(err)
	}
	if !retry.AlreadyInstalled || retry.InstallationID != p.InstallationID {
		t.Fatal("retry duplicated installation")
	}
	activation, err := s.PlanActivate(p.InstallationID, "user:alice", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, activation, activation.Digest); err != nil {
		t.Fatal(err)
	}
	job, err = s.task(p.ScheduleIDs[0])
	if err != nil || !job.Enabled {
		t.Fatal("activation did not enable schedule", err)
	}
	if _, err := s.CheckTask(job); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlanActivate(p.InstallationID, "user:bob", "bob"); err == nil {
		t.Fatal("wrong owner activated installation")
	}
}

func TestSharingRejectsStalePlanAndContentConflict(t *testing.T) {
	node, fsm := newTestRaft(t)
	s := NewSharingStore(node, fsm.Store())
	a := sharingFixture(t)
	p, err := s.PlanInstall(a, "user:alice", map[string]string{"city": "London"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.PlanInstall(a, "user:bob", map[string]string{"city": "Paris"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(context.Background(), other, other.Digest); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(context.Background(), p, p.Digest); err == nil {
		t.Fatal("stale plan applied")
	}
	changed := a.Package()
	changed.Files["SKILL.md"] = []byte("different code")
	b, err := sharing.Build(changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlanInstall(b, "user:alice", map[string]string{"city": "London"}); err == nil {
		t.Fatal("same version conflict accepted")
	}
}

func TestSharingExportExcludesRuntimeOwnership(t *testing.T) {
	node, fsm := newTestRaft(t)
	s := NewSharingStore(node, fsm.Store())
	a := sharingFixture(t)
	p, err := s.PlanInstall(a, "user:alice", map[string]string{"city": "London"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(context.Background(), p, p.Digest); err != nil {
		t.Fatal(err)
	}
	exported, err := s.Export("weather", "0.0.0", p.ScheduleIDs, "user:alice", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(exported.Package().Schedules) != 1 {
		t.Fatal("missing selected schedule")
	}
	if _, err := s.Export("weather", "0.0.0", p.ScheduleIDs, "user:bob", nil, ""); err == nil {
		t.Fatal("exported another owner's schedule")
	}
}

func TestSharingRestoreClearsApproval(t *testing.T) {
	r := &lobslawv1.ShareInstallation{Id: "install", Owner: "user:alice", Active: true, ApprovedRoot: "digest", ApprovedBy: "alice"}
	paused, err := pauseArchiveRecord("skill-installations", r, "UTC")
	if err != nil || !paused || r.Active || r.ApprovedRoot != "" || r.ApprovedBy != "" {
		t.Fatal("restored approval survived", err)
	}
}

func TestSharingArchiveRoundTripAndMissingDependency(t *testing.T) {
	source, fsm := newTestRaft(t)
	s := NewSharingStore(source, fsm.Store())
	ctx := context.Background()
	p, err := s.PlanInstall(sharingFixture(t), "user:alice", map[string]string{"city": "London"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, p, p.Digest); err != nil {
		t.Fatal(err)
	}
	activation, err := s.PlanActivate(p.InstallationID, "user:alice", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, activation, activation.Digest); err != nil {
		t.Fatal(err)
	}
	records, err := fsm.Store().ArchiveRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target, targetFSM := newTestRaft(t)
	if _, err := ApplyArchiveImport(ctx, target, targetFSM.Store(), records, ArchiveImportOptions{Owners: map[string]string{"user:alice": "user:bob"}}, nil); err != nil {
		t.Fatal(err)
	}
	restored := NewSharingStore(target, targetFSM.Store())
	rec, err := restored.Installation(p.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Active || rec.ApprovedRoot != "" || rec.Owner != "user:bob" {
		t.Fatal("restore retained approval or wrong owner")
	}
	job, err := restored.task(p.ScheduleIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if job.Enabled {
		t.Fatal("restore enabled job")
	}
	if _, err := restored.PlanActivate(rec.Id, "user:bob", "bob"); err != nil {
		t.Fatal(err)
	}
	if err := validateArchiveDependencies(map[archiveRecordKey]proto.Message{{"skill-installations", rec.Id}: rec}); err == nil {
		t.Fatal("missing shared skill and schedules accepted")
	}
}

func TestSharingApprovalRejectsChangedBindings(t *testing.T) {
	node, fsm := newTestRaft(t)
	s := NewSharingStore(node, fsm.Store())
	ctx := context.Background()
	p, err := s.PlanInstall(sharingFixture(t), "user:alice", map[string]string{"city": "London"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, p, p.Digest); err != nil {
		t.Fatal(err)
	}
	p, err = s.PlanActivate(p.InstallationID, "user:alice", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, p, p.Digest); err != nil {
		t.Fatal(err)
	}
	rec, err := s.Installation(p.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	rec.Inputs["city"] = "Paris"
	raw, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsm.Store().Put(BucketSkillInstallations, rec.Id, raw); err != nil {
		t.Fatal(err)
	}
	job, err := s.task(rec.ScheduleIds[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckTask(job); err == nil {
		t.Fatal("changed bindings retained approval")
	}
}

func TestSharingInvalidBatchRollsBackAllWrites(t *testing.T) {
	node, fsm := newTestRaft(t)
	s := NewSharingStore(node, fsm.Store())
	p, err := s.PlanInstall(sharingFixture(t), "user:alice", map[string]string{"city": "London"})
	if err != nil {
		t.Fatal(err)
	}
	p.batch.Mutations = append(p.batch.Mutations, &lobslawv1.ShareMutation{Kind: "unknown", Id: "bad"})
	if err := s.Apply(context.Background(), p, p.Digest); err == nil {
		t.Fatal("invalid batch applied")
	}
	if _, err := s.Installation(p.InstallationID); err == nil {
		t.Fatal("partial installation survived rollback")
	}
	if _, err := s.skill("weather", "0.0.0"); err == nil {
		t.Fatal("partial skill survived rollback")
	}
}
