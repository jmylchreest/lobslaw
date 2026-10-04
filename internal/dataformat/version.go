// Package dataformat defines persisted compatibility independently of transports,
// storage engines and application features. Adapters own locking and publication.
package dataformat

import (
	"context"
	"fmt"
)

const (
	StateVersion    = 2
	LogVersion      = 1
	PhysicalVersion = 1
	// ClusterProtocol changes when committed data cannot be read by all peers.
	// This initial release deliberately requires a coordinated cluster upgrade.
	ClusterProtocol       = "lobslaw-data-v2-teams"
	PreviousStateProtocol = "lobslaw-data-v1-main"
	LegacyMain            = "main-v0"
	LegacyTeams           = "pr348-v0"
	LegacyTeamsEarly      = "pr348-early-v0"
)

type Step[T any] struct {
	From  int
	To    int
	Name  string
	Apply func(context.Context, T) (T, error)
}

// Upgrade accepts only a complete, strictly increasing path. The caller supplies
// an isolated value/copy: this runner never publishes partially migrated data.
func Upgrade[T any](ctx context.Context, value T, from, current int, steps []Step[T]) (T, error) {
	if from < 0 || from > current {
		return value, fmt.Errorf("unsupported data version %d (current %d)", from, current)
	}
	for from < current {
		if err := ctx.Err(); err != nil {
			return value, err
		}
		var selected *Step[T]
		for i := range steps {
			if steps[i].From != from {
				continue
			}
			if selected != nil {
				return value, fmt.Errorf("ambiguous migration from version %d", from)
			}
			selected = &steps[i]
		}
		if selected == nil || selected.To <= from || selected.To > current || selected.Apply == nil {
			return value, fmt.Errorf("no migration path from version %d to %d", from, current)
		}
		var err error
		value, err = selected.Apply(ctx, value)
		if err != nil {
			return value, fmt.Errorf("migration %s: %w", selected.Name, err)
		}
		from = selected.To
	}
	return value, ctx.Err()
}
