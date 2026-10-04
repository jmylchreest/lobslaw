package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/scheduler"
	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func scheduleCron(ctx context.Context, when, zone string) (string, error) {
	expr, err := normaliseToCron(when)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(expr, "TZ=") || strings.HasPrefix(expr, "CRON_TZ=") {
		if zone != "" {
			return "", errors.New("timezone supplied twice")
		}
		return expr, nil
	}
	if zone == "" {
		caller, _ := turn.IdentityFrom(ctx)
		zone = caller.Timezone
	}
	if zone == "" {
		zone = "UTC"
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return "", fmt.Errorf("invalid timezone: %w", err)
	}
	return "CRON_TZ=" + zone + " " + expr, nil
}

func newScheduleUpdateHandler(store *memory.Store, raft memoryRaftApplier) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		for range 3 {
			raw, err := store.Get(memory.BucketScheduledTasks, args["id"])
			if err != nil {
				return nil, 2, errors.New("schedule_update: task not found")
			}
			var rec pb.ScheduledTaskRecord
			if err := proto.Unmarshal(raw, &rec); err != nil {
				return nil, 1, err
			}
			if !ownsSchedule(ctx, &rec) || rec.HandlerRef != ScheduleHandlerRef {
				return nil, 2, errors.New("schedule_update: task not found")
			}
			if rec.Params == nil {
				rec.Params = map[string]string{}
			}
			if v, ok := args["enabled"]; ok {
				b, err := strconv.ParseBool(v)
				if err != nil {
					return nil, 2, err
				}
				rec.Enabled = b
				if b {
					rec.NextRun = nil
				}
			}
			if v, ok := args["prompt"]; ok {
				if strings.TrimSpace(v) == "" || len(v) > 8000 {
					return nil, 2, errors.New("invalid prompt")
				}
				rec.Params["prompt"] = v
			}
			if v, ok := args["checkpoint"]; ok {
				if len(v) > 4000 {
					return nil, 2, errors.New("checkpoint exceeds 4000 bytes")
				}
				rec.Params["checkpoint"] = v
			}
			if v, ok := args["notify_on"]; ok {
				if v != "always" && v != "match" && v != "never" {
					return nil, 2, errors.New("invalid notify_on")
				}
				rec.Params["notify_on"] = v
			}
			if when, ok := args["when"]; ok {
				rec.Schedule, err = scheduleCron(ctx, when, args["timezone"])
				if err != nil {
					return nil, 2, err
				}
				rec.NextRun = nil
			} else if args["timezone"] != "" {
				return nil, 2, errors.New("supply when with timezone")
			}
			if rec.NextRun == nil {
				parsed, err := scheduler.ParseAgentSchedule(rec.Schedule)
				if err != nil {
					return nil, 2, err
				}
				rec.NextRun = timestamppb.New(parsed.Next(time.Now()))
			}
			entry := &pb.LogEntry{Op: pb.LogOp_LOG_OP_CLAIM, Id: rec.Id, ExpectedRevision: &rec.Revision, ExpectedClaimer: rec.ClaimedBy, Payload: &pb.LogEntry_ScheduledTask{ScheduledTask: &rec}}
			data, err := proto.Marshal(entry)
			if err != nil {
				return nil, 1, err
			}
			if _, err = raft.Apply(data, 5*time.Second); errors.Is(err, memory.ErrClaimConflict) {
				continue
			} else if err != nil {
				return nil, 1, err
			}
			out, _ := json.Marshal(map[string]any{"id": rec.Id, "enabled": rec.Enabled, "updated": true})
			return out, 0, nil
		}
		return nil, 1, errors.New("schedule changed concurrently; reload and retry")
	}
}

func ownsSchedule(ctx context.Context, rec *pb.ScheduledTaskRecord) bool {
	if !ownedByCaller(ctx, rec.Owner) {
		return false
	}
	if owner := rec.Params["requested_by"]; owner != "" {
		caller, _ := turn.IdentityFrom(ctx)
		return requesterLabel(caller) == owner
	}
	return true
}
