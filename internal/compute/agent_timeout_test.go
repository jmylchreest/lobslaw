package compute

import (
	"context"
	"errors"
	"testing"
	"time"
)

type deadlineProvider func(context.Context, ChatRequest) (*ChatResponse, error)

func (p deadlineProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	return p(ctx, req)
}

func TestMainRoleDeadline(t *testing.T) {
	t.Parallel()
	for _, registry := range []bool{false, true} {
		for _, configured := range []time.Duration{0, 5 * time.Minute} {
			for _, parentTimeout := range []time.Duration{0, time.Second, 5 * time.Minute} {
				t.Run(configured.String()+"/"+parentTimeout.String()+"/registry="+map[bool]string{false: "no", true: "yes"}[registry], func(t *testing.T) {
					t.Parallel()
					p := deadlineProvider(func(ctx context.Context, _ ChatRequest) (*ChatResponse, error) {
						deadline, ok := ctx.Deadline()
						want := orDefault(configured, DefaultLLMTimeout)
						if parentTimeout > 0 && (configured <= 0 || parentTimeout < want) {
							want = parentTimeout
						}
						if !ok || time.Until(deadline) <= want-time.Second/2 || time.Until(deadline) > want {
							t.Errorf("main deadline in %v (present=%v), want %v", time.Until(deadline), ok, want)
						}
						return &ChatResponse{Content: "ok"}, nil
					})
					rm, err := NewRoleMap(p, nil)
					if err != nil {
						t.Fatal(err)
					}
					rm.SetTimeouts(0, map[Role]time.Duration{RoleMain: configured})
					a := &Agent{cfg: AgentConfig{Provider: p, Roles: rm, Logger: quietLog()}}
					if registry {
						a.cfg.Providers = NewProviderRegistry()
						a.cfg.Providers.Register(ProviderEntry{Label: "main", Client: p})
						a.cfg.PrimaryLabel = "main"
					}
					ctx := context.Background()
					if parentTimeout > 0 {
						var cancel context.CancelFunc
						ctx, cancel = context.WithTimeout(ctx, parentTimeout)
						defer cancel()
					}
					if _, err := a.dispatchWithBackup(ctx, ChatRequest{}); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func TestMainRoleTimeoutAdvancesBackup(t *testing.T) {
	t.Parallel()
	for _, cancelParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "attempt deadline", true: "parent cancellation"}[cancelParent], func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			primary := deadlineProvider(func(callCtx context.Context, _ ChatRequest) (*ChatResponse, error) {
				if _, ok := callCtx.Deadline(); !ok {
					return nil, errors.New("main call has no deadline")
				}
				if cancelParent {
					cancel()
				}
				<-callCtx.Done()
				return nil, Permanent(callCtx.Err())
			})
			backupCalled := false
			backup := deadlineProvider(func(callCtx context.Context, _ ChatRequest) (*ChatResponse, error) {
				backupCalled = true
				if callCtx.Err() != nil {
					t.Error("backup inherited expired attempt context")
				}
				return &ChatResponse{Content: "backup"}, nil
			})
			rm, err := NewRoleMap(primary, nil)
			if err != nil {
				t.Fatal(err)
			}
			rm.SetTimeouts(0, map[Role]time.Duration{RoleMain: 20 * time.Millisecond})
			reg := NewProviderRegistry()
			reg.Register(ProviderEntry{Label: "primary", Client: primary, Backup: "backup"})
			reg.Register(ProviderEntry{Label: "backup", Client: backup})
			a := &Agent{cfg: AgentConfig{Roles: rm, Providers: reg, PrimaryLabel: "primary", Logger: quietLog()}}
			resp, err := a.dispatchWithBackup(ctx, ChatRequest{})
			if cancelParent {
				if backupCalled || !errors.Is(err, context.Canceled) {
					t.Fatalf("parent cancellation: backup=%v err=%v", backupCalled, err)
				}
			} else if err != nil || !backupCalled || resp.resp.Content != "backup" {
				t.Fatalf("attempt timeout should fail over: backup=%v resp=%v err=%v", backupCalled, resp, err)
			}
		})
	}
}

func TestSummariserRoleDeadline(t *testing.T) {
	t.Parallel()
	provider := deadlineProvider(func(ctx context.Context, _ ChatRequest) (*ChatResponse, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 4*time.Second || time.Until(deadline) > 5*time.Second {
			t.Errorf("summariser deadline in %v (present=%v), want about 5s", time.Until(deadline), ok)
		}
		return &ChatResponse{Content: "durable fact"}, nil
	})
	rm, err := NewRoleMap(provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	rm.SetTimeouts(0, map[Role]time.Duration{RoleSummariser: 5 * time.Second})
	s := NewDreamSummarizer(rm.ForWithTimeout(RoleSummariser), "m", quietLog(), nil)
	if _, _, err := s.Summarize(context.Background(), []string{"event"}); err != nil {
		t.Fatal(err)
	}
}
