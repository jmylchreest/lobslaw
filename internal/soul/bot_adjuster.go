package soul

import (
	"context"
	"errors"
)

// ForBot binds every operation, including rollback, to the same identity as
// prompt assembly. A fresh view avoids a shared mutable "current bot" and
// picks up the latest operator baseline without mixing another bot's cache.
func (a *Adjuster) ForBot(botID string) (*Adjuster, error) {
	if botID == "" || botID == "chief" {
		return a, nil
	}
	store, ok := a.store.(BotTuneWriter)
	if !ok {
		return nil, errors.New("soul: per-bot tuning is unavailable")
	}
	a.mu.RLock()
	baseline := *a.baseline
	a.mu.RUnlock()
	return NewAdjuster(AdjusterConfig{Soul: &baseline, Store: boundBotTuneStore{store, botID}, DeferLoad: true})
}

type boundBotTuneStore struct {
	store BotTuneWriter
	id    string
}

func (s boundBotTuneStore) Get(ctx context.Context) (*TuneState, error) {
	return s.store.GetFor(ctx, s.id)
}

func (s boundBotTuneStore) Put(ctx context.Context, state *TuneState) error {
	return s.store.PutFor(ctx, s.id, state)
}

func (s boundBotTuneStore) Rollback(ctx context.Context, steps int) (*TuneState, error) {
	return s.store.RollbackFor(ctx, s.id, steps)
}
