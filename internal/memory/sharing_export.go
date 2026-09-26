package memory

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/sharing"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Export selects records inside one consistent read transaction. No knowledge
// archive or filesystem walk is involved, so unrelated records cannot leak.
func (s *SharingStore) Export(name, version string, ids []string, owner string, inputs []string, timezone string) (sharing.Artifact, error) {
	snap, err := s.snapshot()
	if err != nil {
		return sharing.Artifact{}, err
	}
	rec := new(lobslawv1.SkillRecord)
	if version != "" {
		if err := snap.read("skills", SkillKey(name, version), rec); err != nil {
			return sharing.Artifact{}, err
		}
	} else {
		for key, raw := range snap {
			if !strings.HasPrefix(key, "skills/") {
				continue
			}
			candidate := new(lobslawv1.SkillRecord)
			if err := proto.Unmarshal(raw, candidate); err != nil {
				return sharing.Artifact{}, err
			}
			if candidate.Name == name && candidate.Active {
				if rec.Name != "" {
					return sharing.Artifact{}, errors.New("sharing: multiple active versions; select a version explicitly")
				}
				rec = candidate
			}
		}
		if rec.Name == "" {
			return sharing.Artifact{}, errors.New("sharing: no active stored skill; select a version explicitly")
		}
	}
	p := sharing.Package{Format: sharing.Format, Schema: 1, Name: rec.Name, Version: rec.Version, Manifest: rec.ManifestYaml, ManifestSignature: rec.ManifestSig, Files: make(map[string][]byte), Inputs: inputs}
	for path, digest := range rec.Files {
		blob := new(lobslawv1.SkillBlob)
		if err := snap.read("skill-blobs", digest, blob); err != nil {
			return sharing.Artifact{}, err
		}
		if Digest(blob.Content) != digest {
			return sharing.Artifact{}, errors.New("sharing: corrupt skill payload")
		}
		p.Files[path] = blob.Content
	}
	seen := make(map[string]bool)
	for i, id := range ids {
		if seen[id] {
			return sharing.Artifact{}, errors.New("sharing: duplicate schedule selection")
		}
		seen[id] = true
		task := new(lobslawv1.ScheduledTaskRecord)
		if err := snap.read("scheduled-tasks", id, task); err != nil {
			return sharing.Artifact{}, err
		}
		if owner == "" || task.Owner != owner {
			return sharing.Artifact{}, errors.New("sharing: cannot export another owner's schedule")
		}
		if task.HandlerRef != "agent:turn" {
			return sharing.Artifact{}, errors.New("sharing: only agent:turn schedules are portable")
		}
		for key := range task.Params {
			switch key {
			case "prompt", "notify_on", "share_installation":
			default:
				return sharing.Artifact{}, fmt.Errorf("sharing: schedule parameter %q needs an explicit portable binding; refusing to discard it", key)
			}
		}
		cron, tz := task.Schedule, timezone
		if strings.HasPrefix(cron, "CRON_TZ=") || strings.HasPrefix(cron, "TZ=") {
			prefix, rest, ok := strings.Cut(cron, " ")
			if !ok {
				return sharing.Artifact{}, errors.New("sharing: malformed cron timezone")
			}
			_, tz, _ = strings.Cut(prefix, "=")
			cron = rest
		}
		notify := task.Params["notify_on"]
		if notify == "" {
			notify = "match"
		}
		p.Schedules = append(p.Schedules, sharing.Schedule{Key: fmt.Sprintf("schedule-%d", i+1), Name: task.Name, Cron: cron, Timezone: tz, Prompt: task.Params["prompt"], NotifyOn: notify})
	}
	return sharing.Build(p)
}
