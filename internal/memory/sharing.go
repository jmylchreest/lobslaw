package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/sharing"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type SharingStore struct {
	raft  raftApplier
	store *Store
}

func NewSharingStore(raft raftApplier, store *Store) *SharingStore {
	return &SharingStore{raft: raft, store: store}
}

type SharePlan struct {
	InstallationID   string             `json:"installation_id"`
	Owner            string             `json:"owner"`
	Name             string             `json:"name"`
	Version          string             `json:"version"`
	ContentDigest    string             `json:"content_digest"`
	Digest           string             `json:"plan_digest"`
	ScheduleIDs      []string           `json:"schedule_ids"`
	Schedules        []sharing.Schedule `json:"schedules"`
	Activation       bool               `json:"activation"`
	AlreadyInstalled bool               `json:"already_installed"`
	AlreadyActive    bool               `json:"already_active"`
	batch            *lobslawv1.ShareBatch
}

var shareKinds = map[string]string{"skills": BucketSkills, "skill-blobs": BucketSkillBlobs, "scheduled-tasks": BucketScheduledTasks, "skill-installations": BucketSkillInstallations}

type shareSnapshot map[string][]byte

func shareKey(kind, id string) string { return kind + "/" + id }

func shareSnapshotTx(store *Store, tx *bolt.Tx) (shareSnapshot, error) {
	out := make(shareSnapshot)
	for kind, bucket := range shareKinds {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			continue
		}
		if err := b.ForEach(func(k, v []byte) error {
			raw, err := store.cipher.OpenTo(nil, v)
			if err != nil {
				return err
			}
			out[shareKey(kind, string(k))] = bytes.Clone(raw)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *SharingStore) snapshot() (shareSnapshot, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("sharing: store unavailable")
	}
	var out shareSnapshot
	err := s.store.loadDB().View(func(tx *bolt.Tx) error { var err error; out, err = shareSnapshotTx(s.store, tx); return err })
	return out, err
}

func (snap shareSnapshot) digest() string {
	raw, _ := json.Marshal(snap) // map[string][]byte is always JSON encodable.
	return sharing.Hash(raw)
}
func (snap shareSnapshot) read(kind, id string, p proto.Message) error {
	raw, ok := snap[shareKey(kind, id)]
	if !ok {
		return types.ErrNotFound
	}
	return proto.Unmarshal(raw, p)
}
func (s *SharingStore) Installation(id string) (*lobslawv1.ShareInstallation, error) {
	snap, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	rec := new(lobslawv1.ShareInstallation)
	err = snap.read("skill-installations", id, rec)
	return rec, err
}
func (s *SharingStore) skill(name, version string) (*lobslawv1.SkillRecord, error) {
	snap, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	rec := new(lobslawv1.SkillRecord)
	err = snap.read("skills", SkillKey(name, version), rec)
	return rec, err
}
func (s *SharingStore) task(id string) (*lobslawv1.ScheduledTaskRecord, error) {
	snap, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	rec := new(lobslawv1.ScheduledTaskRecord)
	err = snap.read("scheduled-tasks", id, rec)
	return rec, err
}

func artifactSkill(a sharing.Artifact) *lobslawv1.SkillRecord {
	p := a.Package()
	files := make(map[string]string, len(p.Files))
	for path, raw := range p.Files {
		files[path] = Digest(raw)
	}
	tier := lobslawv1.SkillTier_SKILL_TIER_OPERATOR
	if len(p.ManifestSignature) > 0 {
		tier = lobslawv1.SkillTier_SKILL_TIER_SIGNED
	}
	return &lobslawv1.SkillRecord{Name: p.Name, Version: p.Version, ManifestYaml: p.Manifest, ManifestSig: p.ManifestSignature, Files: files, Tier: tier, Source: "share:" + a.Digest()}
}

func sameSharedSkill(a, b *lobslawv1.SkillRecord) bool {
	return a.Name == b.Name && a.Version == b.Version && bytes.Equal(a.ManifestYaml, b.ManifestYaml) && bytes.Equal(a.ManifestSig, b.ManifestSig) && equalStringMap(a.Files, b.Files) && a.Tier == b.Tier
}
func equalStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func addShareMutation(batch *lobslawv1.ShareBatch, kind, id string, msg proto.Message) error {
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(msg)
	if err != nil {
		return err
	}
	batch.Mutations = append(batch.Mutations, &lobslawv1.ShareMutation{Kind: kind, Id: id, Payload: raw})
	return nil
}

func finishSharePlan(p *SharePlan) error {
	sort.Slice(p.batch.Mutations, func(i, j int) bool {
		a, b := p.batch.Mutations[i], p.batch.Mutations[j]
		return shareKey(a.Kind, a.Id) < shareKey(b.Kind, b.Id)
	})
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(p.batch)
	if err != nil {
		return err
	}
	if len(raw) > 4<<20 {
		return errors.New("sharing: installation exceeds transaction limit")
	}
	p.Digest = sharing.Hash(raw)
	return nil
}

func (s *SharingStore) PlanInstall(a sharing.Artifact, owner string, inputs map[string]string) (*SharePlan, error) {
	if len(inputs) == 0 {
		inputs = nil
	}
	if !strings.HasPrefix(owner, "user:") || len(strings.TrimPrefix(owner, "user:")) == 0 {
		return nil, errors.New("sharing: a nonempty user principal is required")
	}
	if _, err := sharing.Decode(a.Bytes()); err != nil {
		return nil, err
	}
	bound, err := sharing.Bind(a.Package(), inputs)
	if err != nil {
		return nil, err
	}
	snap, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	identity, _ := json.Marshal(struct {
		Digest, Owner string
		Inputs        map[string]string
	}{a.Digest(), owner, inputs})
	id := "share-" + strings.TrimPrefix(sharing.Hash(identity), "sha256:")
	rec := &lobslawv1.ShareInstallation{Id: id, Owner: owner, Artifact: a.Bytes(), Inputs: inputs, Revision: 1}
	for _, job := range bound {
		rec.ScheduleIds = append(rec.ScheduleIds, id+":"+job.Key)
	}
	p := &SharePlan{InstallationID: id, Owner: owner, Name: a.Package().Name, Version: a.Package().Version, ContentDigest: a.Digest(), ScheduleIDs: rec.ScheduleIds, Schedules: bound, batch: &lobslawv1.ShareBatch{ExpectedState: snap.digest()}}
	want := artifactSkill(a)
	old := new(lobslawv1.SkillRecord)
	err = snap.read("skills", SkillKey(want.Name, want.Version), old)
	if err == nil && !sameSharedSkill(old, want) {
		return nil, errors.New("sharing: installed name/version has different content or trust tier")
	}
	if err != nil && !errors.Is(err, types.ErrNotFound) {
		return nil, err
	}
	existing := new(lobslawv1.ShareInstallation)
	if readErr := snap.read("skill-installations", id, existing); readErr == nil {
		if existing.Owner != owner || !equalStringMap(existing.Inputs, inputs) || !bytes.Equal(existing.Artifact, rec.Artifact) {
			return nil, errors.New("sharing: installation identity conflict")
		}
		if err != nil {
			return nil, errors.New("sharing: installed skill was removed; refusing to recreate it")
		}
		if err := checkSharedTasks(snap, existing, bound, false); err != nil {
			return nil, err
		}
		p.AlreadyInstalled = true
		p.AlreadyActive = existing.Active
		return p, finishSharePlan(p)
	} else if !errors.Is(readErr, types.ErrNotFound) {
		return nil, readErr
	}
	if err != nil {
		want.ImportedBy = owner
		want.Revision = 1
		if err := addShareMutation(p.batch, "skills", SkillKey(want.Name, want.Version), want); err != nil {
			return nil, err
		}
	}
	for path, raw := range a.Package().Files {
		key := want.Files[path]
		blob := new(lobslawv1.SkillBlob)
		if err := snap.read("skill-blobs", key, blob); err == nil {
			if !bytes.Equal(blob.Content, raw) {
				return nil, errors.New("sharing: stored blob digest collision")
			}
			continue
		} else if !errors.Is(err, types.ErrNotFound) {
			return nil, err
		}
		if err := addShareMutation(p.batch, "skill-blobs", key, &lobslawv1.SkillBlob{Digest: key, Content: raw, Revision: 1}); err != nil {
			return nil, err
		}
	}
	for i, job := range bound {
		task := sharedTask(rec, i, job)
		if _, ok := snap[shareKey("scheduled-tasks", task.Id)]; ok {
			return nil, errors.New("sharing: schedule identity already exists")
		}
		if err := addShareMutation(p.batch, "scheduled-tasks", task.Id, task); err != nil {
			return nil, err
		}
	}
	if err := addShareMutation(p.batch, "skill-installations", id, rec); err != nil {
		return nil, err
	}
	return p, finishSharePlan(p)
}

func sharedTask(rec *lobslawv1.ShareInstallation, i int, job sharing.Schedule) *lobslawv1.ScheduledTaskRecord {
	return &lobslawv1.ScheduledTaskRecord{Id: rec.ScheduleIds[i], Name: job.Name, Schedule: "CRON_TZ=" + job.Timezone + " " + job.Cron, HandlerRef: "agent:turn", Owner: rec.Owner, CreatedBy: strings.TrimPrefix(rec.Owner, "user:"), Revision: 1, Params: map[string]string{"prompt": job.Prompt, "notify_on": job.NotifyOn, "share_installation": rec.Id}}
}

func checkSharedTasks(snap shareSnapshot, rec *lobslawv1.ShareInstallation, bound []sharing.Schedule, active bool) error {
	if len(rec.ScheduleIds) != len(bound) {
		return errors.New("sharing: installation schedule references mismatch")
	}
	for i, job := range bound {
		task := new(lobslawv1.ScheduledTaskRecord)
		if err := snap.read("scheduled-tasks", rec.ScheduleIds[i], task); err != nil {
			return fmt.Errorf("sharing: schedule removed: %w", err)
		}
		want := sharedTask(rec, i, job)
		if task.Owner != want.Owner || task.Name != want.Name || task.Schedule != want.Schedule || task.HandlerRef != want.HandlerRef || !equalStringMap(task.Params, want.Params) || (active && !task.Enabled) {
			return errors.New("sharing: schedule changed; export and review a new installation")
		}
	}
	return nil
}

func (s *SharingStore) PlanActivate(id, owner, actor string) (*SharePlan, error) {
	snap, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	rec := new(lobslawv1.ShareInstallation)
	if err := snap.read("skill-installations", id, rec); err != nil {
		return nil, err
	}
	if owner == "" || owner != rec.Owner || actor == "" {
		return nil, errors.New("sharing: installation owner and authenticated approver required")
	}
	a, err := sharing.Decode(rec.Artifact)
	if err != nil {
		return nil, err
	}
	bound, err := sharing.Bind(a.Package(), rec.Inputs)
	if err != nil {
		return nil, err
	}
	if err := checkSharedTasks(snap, rec, bound, false); err != nil {
		return nil, err
	}
	want := artifactSkill(a)
	skill := new(lobslawv1.SkillRecord)
	if err := snap.read("skills", SkillKey(want.Name, want.Version), skill); err != nil {
		return nil, err
	}
	if !sameSharedSkill(want, skill) {
		return nil, errors.New("sharing: skill content changed since installation")
	}
	for key, raw := range snap {
		if strings.HasPrefix(key, "skills/") {
			other := new(lobslawv1.SkillRecord)
			if err := proto.Unmarshal(raw, other); err != nil {
				return nil, err
			}
			if other.Name == skill.Name && other.Version != skill.Version && other.Active {
				return nil, errors.New("sharing: another version is active; resolve it explicitly before activation")
			}
		}
	}
	for path, digest := range skill.Files {
		blob := new(lobslawv1.SkillBlob)
		if err := snap.read("skill-blobs", digest, blob); err != nil {
			return nil, err
		}
		if !bytes.Equal(blob.Content, a.Package().Files[path]) {
			return nil, errors.New("sharing: skill blob changed")
		}
	}
	p := &SharePlan{InstallationID: id, Owner: owner, Name: skill.Name, Version: skill.Version, ContentDigest: a.Digest(), ScheduleIDs: rec.ScheduleIds, Schedules: bound, Activation: true, AlreadyInstalled: true, AlreadyActive: rec.Active, batch: &lobslawv1.ShareBatch{ExpectedState: snap.digest()}}
	if rec.Active {
		if !skill.Active {
			return nil, errors.New("sharing: approved skill was deactivated")
		}
		if rec.ApprovedRoot != shareApprovalRoot(rec) {
			return nil, errors.New("sharing: approval no longer matches content")
		}
		if err := checkSharedTasks(snap, rec, bound, true); err != nil {
			return nil, err
		}
		return p, finishSharePlan(p)
	}
	if !skill.Active {
		skill.Active = true
		skill.Revision++
		if err := addShareMutation(p.batch, "skills", SkillKey(skill.Name, skill.Version), skill); err != nil {
			return nil, err
		}
	}
	for _, taskID := range rec.ScheduleIds {
		task := new(lobslawv1.ScheduledTaskRecord)
		if err := snap.read("scheduled-tasks", taskID, task); err != nil {
			return nil, err
		}
		task.Enabled = true
		task.NextRun = nil
		task.ClaimedBy = ""
		task.ClaimExpiresAt = nil
		task.Revision++
		if err := addShareMutation(p.batch, "scheduled-tasks", taskID, task); err != nil {
			return nil, err
		}
	}
	rec.Active = true
	rec.ApprovedRoot = shareApprovalRoot(rec)
	rec.ApprovedBy = actor
	rec.Revision++
	if err := addShareMutation(p.batch, "skill-installations", rec.Id, rec); err != nil {
		return nil, err
	}
	return p, finishSharePlan(p)
}

func (s *SharingStore) Apply(ctx context.Context, p *SharePlan, expected string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || expected == "" || expected != p.Digest || p.batch == nil {
		return errors.New("sharing: expected-plan must match a fresh preview")
	}
	if s.raft == nil {
		return errors.New("sharing: raft unavailable")
	}
	batch := proto.Clone(p.batch).(*lobslawv1.ShareBatch)
	for _, m := range batch.Mutations {
		if m.Kind == "skill-installations" {
			rec := new(lobslawv1.ShareInstallation)
			if err := proto.Unmarshal(m.Payload, rec); err != nil {
				return err
			}
			if rec.Active {
				rec.ApprovedAt = timestamppb.Now()
				raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(rec)
				if err != nil {
					return err
				}
				m.Payload = raw
			}
		}
	}
	entry := &lobslawv1.LogEntry{Op: lobslawv1.LogOp_LOG_OP_PUT, Payload: &lobslawv1.LogEntry_ShareBatch{ShareBatch: batch}}
	raw, err := proto.Marshal(entry)
	if err != nil {
		return err
	}
	res, err := s.raft.Apply(raw, 5*time.Second)
	if err != nil {
		return err
	}
	if applyErr, ok := res.(error); ok {
		return applyErr
	}
	return nil
}

// CheckTask refuses stale approvals at execution, even if someone enabled a
// staged task via another API. Normal policy and command-risk gates still run.
func (s *SharingStore) CheckTask(task *lobslawv1.ScheduledTaskRecord) (sharing.Artifact, error) {
	snap, err := s.snapshot()
	if err != nil {
		return sharing.Artifact{}, err
	}
	rec := new(lobslawv1.ShareInstallation)
	if err := snap.read("skill-installations", task.Params["share_installation"], rec); err != nil {
		return sharing.Artifact{}, err
	}
	a, err := sharing.Decode(rec.Artifact)
	if err != nil {
		return a, err
	}
	if !rec.Active || rec.ApprovedRoot != shareApprovalRoot(rec) || rec.Owner != task.Owner {
		return a, errors.New("sharing: schedule has no current activation approval")
	}
	bound, err := sharing.Bind(a.Package(), rec.Inputs)
	if err != nil {
		return a, err
	}
	if err := checkSharedTasks(snap, rec, bound, true); err != nil {
		return a, err
	}
	found := false
	for i, id := range rec.ScheduleIds {
		if id == task.Id {
			want := sharedTask(rec, i, bound[i])
			found = task.Name == want.Name && task.Schedule == want.Schedule && task.HandlerRef == want.HandlerRef && equalStringMap(task.Params, want.Params)
		}
	}
	if !found {
		return a, errors.New("sharing: task does not match approved installation")
	}
	sk := new(lobslawv1.SkillRecord)
	if err := snap.read("skills", SkillKey(a.Package().Name, a.Package().Version), sk); err != nil {
		return a, err
	}
	if !sk.Active || !sameSharedSkill(sk, artifactSkill(a)) {
		return a, errors.New("sharing: approved skill is no longer active or unchanged")
	}
	return a, nil
}

// Approval includes local bindings as well as portable content.
func shareApprovalRoot(rec *lobslawv1.ShareInstallation) string {
	// This fixed shape contains only strings, bytes and string maps/slices;
	// JSON encoding cannot fail. Revisit error handling if these types change.
	raw, _ := json.Marshal(struct {
		Artifact  []byte
		Owner     string
		Inputs    map[string]string
		Schedules []string
	}{rec.Artifact, rec.Owner, rec.Inputs, rec.ScheduleIds})
	return sharing.Hash(raw)
}
