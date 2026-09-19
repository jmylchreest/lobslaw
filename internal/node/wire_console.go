package node

import (
	"context"
	"slices"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/config"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// A compute backend serves the browser channel over cluster mTLS even when its
// public HTTP channels are disabled. It neither binds a listener nor starts
// Telegram/Slack; the web node owns those transport and login concerns.
func (n *Node) wireConsoleBackend() error {
	if n.cfg.RestoreMode || n.gatewaySrv != nil || n.agent == nil || n.server == nil {
		return nil
	}
	if err := n.wirePrompts(); err != nil {
		return err
	}
	server := gateway.NewServer(gateway.RESTConfig{
		Computer:    n.computer,
		RequireAuth: true, Identity: n.identityResolver(), Logger: n.log,
		DefaultScope:  n.cfg.Gateway.UnknownUserScope,
		DefaultBudget: compute.FromComputeConfig(n.cfg.Compute),
		QueueMode:     gateway.ParseQueueMode(n.cfg.Gateway.QueueMode),
		QueueDebounce: n.cfg.Gateway.QueueDebounce, Leaser: n.newSessionLeaser(),
		TypingInterval: n.cfg.Gateway.TypingInterval, HardTimeout: n.cfg.Gateway.HardTimeout,
		Prompts: n.promptRegistry, ConfirmationTTL: n.cfg.Gateway.ConfirmationTimeout,
		Bots: n.teamBotsOrNil(), Groups: n.teamGroupsOrNil(), Inbox: n.teamInboxOrNil(),
		TeamRouter: n.teamRouterOrNil(), Tools: n.toolCatalogueOrNil(),
		Sessions: n.newSessionStore(), Compactor: n.newSessionCompactor(), Conversation: n.conversationConfig(),
		Transcripts: n.newSessionBrowser(), Routines: n.newRoutineLister(), Memory: n.newMemoryLister(),
		Plan: planServiceOrNil(n.planSvc),
	}, compute.Adapt(n.agent))
	lobslawv1.RegisterConsoleServiceServer(n.server, server)
	return nil
}

// Console wiring for the full web console's read-only views.
//
// Each adapter is a gateway-local interface satisfied here at the
// wiring layer, for the same reason sessionStoreAdapter is: the
// gateway must not import memory, the scheduler or compute to render a
// list. The node translates shapes; the gateway only speaks HTTP.

// consoleSessionBrowser exposes the read side of the session service
// to /v1/bots/{id}/sessions and /v1/sessions/{id}. Distinct from
// sessionBrowserAdapter, which serves the agent's session_search tools
// with a visibility predicate. The gateway authorizes the session metadata
// before calling this adapter's transcript reader.
type consoleSessionBrowser struct {
	inner *memory.SessionService
}

func (a *consoleSessionBrowser) ListFiltered(ctx context.Context, channel, userID string) ([]*lobslawv1.SessionRecord, error) {
	return a.inner.ListFiltered(ctx, channel, userID)
}

func (a *consoleSessionBrowser) LoadMessages(ctx context.Context, id string) ([]*lobslawv1.SessionMessage, error) {
	return a.inner.LoadMessages(ctx, id)
}

// newSessionBrowser returns a gateway.SessionBrowser backed by this
// node's transcript store, or nil when it hosts none.
func (n *Node) newSessionBrowser() gateway.SessionBrowser {
	if n.raft == nil || n.store == nil {
		return nil
	}
	return &consoleSessionBrowser{inner: memory.NewSessionService(n.raft, n.store, memory.SessionConfig{
		MaxMessages: n.cfg.Gateway.SessionMaxMessages,
	})}
}

// routineAdapter answers "what scheduled work does this principal
// own" by scanning the replicated task bucket. Read-only, so it is
// safe on a follower where a write would not be.
type routineAdapter struct {
	store *memory.Store
}

func (a *routineAdapter) TasksForOwner(owner string) ([]*lobslawv1.ScheduledTaskRecord, error) {
	var out []*lobslawv1.ScheduledTaskRecord
	err := a.store.ForEach(memory.BucketScheduledTasks, func(_ string, raw []byte) error {
		var rec lobslawv1.ScheduledTaskRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return nil
		}
		if rec.GetOwner() != owner {
			return nil
		}
		out = append(out, &rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetId() < out[j].GetId() })
	return out, nil
}

// newRoutineLister returns a gateway.RoutineAPI, or nil on a node with
// no replicated store to read.
func (n *Node) newRoutineLister() gateway.RoutineAPI {
	if n.store == nil {
		return nil
	}
	return &routineAdapter{store: n.store}
}

// memoryAdapter answers "what does this principal remember" by
// scanning the episodic bucket. Same read-only reasoning as routines.
type memoryAdapter struct {
	store *memory.Store
}

func (a *memoryAdapter) RecordsForOwner(_ context.Context, owner string, limit int) ([]gateway.MemoryRecordView, int, error) {
	type row struct {
		view gateway.MemoryRecordView
		at   time.Time
	}
	var all []row
	err := a.store.ForEach(memory.BucketEpisodicRecords, func(_ string, raw []byte) error {
		var rec lobslawv1.EpisodicRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return nil
		}
		if rec.GetOwner() != owner {
			return nil
		}
		view := gateway.MemoryRecordView{
			ID:    rec.GetId(),
			Kind:  "episodic",
			Text:  rec.GetEvent(),
			Tags:  rec.GetTags(),
			Scope: types.RetentionString(rec.GetRetention()),
		}
		var at time.Time
		if ts := rec.GetTimestamp(); ts != nil {
			at = ts.AsTime()
			view.CreatedAt = at.UTC().Format(time.RFC3339)
		}
		all = append(all, row{view: view, at: at})
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	total := len(all)
	out := make([]gateway.MemoryRecordView, 0, len(all))
	for _, r := range all {
		out = append(out, r.view)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, total, nil
}

// newMemoryLister returns a gateway.MemoryAPI, or nil on a node with
// no replicated store.
func (n *Node) newMemoryLister() gateway.MemoryAPI {
	if n.store == nil {
		return nil
	}
	return &memoryAdapter{store: n.store}
}

// consoleConfigView assembles the allowlisted view /v1/config serves.
//
// Field by field, deliberately: a denylist over the marshalled config
// leaks whatever nobody remembered to redact, and endpoints,
// credentials and secret references are exactly the things nobody is
// thinking about when they add a field.
func (n *Node) consoleConfigView() *gateway.ConfigView {
	functions := make([]string, 0, len(n.cfg.Functions))
	for _, f := range n.cfg.Functions {
		functions = append(functions, string(f))
	}

	roles := providerRolesByLabel(n.cfg.Compute.Roles)
	providers := make([]gateway.ConfigProviderRow, 0, len(n.cfg.Compute.Providers))
	for _, p := range n.cfg.Compute.Providers {
		providers = append(providers, gateway.ConfigProviderRow{
			Label:     p.Label,
			TrustTier: p.TrustTier.String(),
			Roles:     roles[p.Label],
		})
	}

	channels := make([]gateway.ConfigChannelRow, 0, len(n.cfg.Gateway.Channels))
	for _, ch := range n.cfg.Gateway.Channels {
		channels = append(channels, gateway.ConfigChannelRow{Type: ch.Type, Enabled: true})
	}

	memoryOn := slices.Contains(n.cfg.Functions, types.FunctionMemory)
	teamsOn := slices.Contains(n.cfg.Functions, types.FunctionComputeTeams)

	return &gateway.ConfigView{
		NodeID:    n.cfg.NodeID,
		Version:   n.cfg.Version,
		Functions: functions,
		Gateway: gateway.ConfigGatewayView{
			Enabled:         n.cfg.Gateway.Enabled,
			HTTPPort:        n.cfg.Gateway.HTTPPort,
			RequireAuth:     n.cfg.Auth.RequireAuth,
			UIEnabled:       slices.Contains(n.cfg.Functions, types.FunctionUIWeb),
			DefaultTimezone: n.cfg.Gateway.DefaultTimezone,
			QueueMode:       n.cfg.Gateway.QueueMode,
		},
		Compute: gateway.ConfigComputeView{
			Providers:        providers,
			MaxToolCalls:     n.cfg.Compute.Limits.MaxToolCallsPerTurn,
			SelfLearningMode: n.cfg.SelfLearningMode,
		},
		Memory: gateway.ConfigMemoryView{
			Enabled:        memoryOn,
			DreamSchedule:  n.cfg.MemoryDream.Schedule,
			EmbeddingModel: n.cfg.Compute.Embeddings.Model,
		},
		Bots: gateway.ConfigBotsView{
			MaxPending:   memory.DefaultInboxMaxPending,
			DrainEnabled: teamsOn,
		},
		Channels: channels,
	}
}

// providerRolesByLabel inverts [compute.roles] so each provider row can
// list the roles it serves. Labels only — never the endpoints.
func providerRolesByLabel(r config.RolesConfig) map[string][]string {
	out := map[string][]string{}
	add := func(role, label string) {
		if label == "" {
			return
		}
		out[label] = append(out[label], role)
	}
	add("main", r.Main.Provider)
	add("preflight", r.Preflight.Provider)
	add("summariser", r.Summariser.Provider)
	add("review", r.Review.Provider)
	add("command_risk", r.CommandRisk.Provider)
	for label := range out {
		sort.Strings(out[label])
	}
	return out
}
