package notify

import (
	"context"
	"time"
)

// Event is a selective, owner-scoped notification derived from durable work.
// Transports share selection and identity; they choose their own presentation.
type Event struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	Icon      string    `json:"icon"`
	At        time.Time `json:"-"`
	Attention bool      `json:"-"`
	Expires   time.Time `json:"-"`
	BotID     string    `json:"-"`
	TaskID    string    `json:"-"`
	InboxID   string    `json:"-"`
}

type EventSource func(context.Context, string) ([]Event, error)

// EventPage bounds a delivery read. Next is an opaque owner-scoped cursor.
type EventPage struct {
	Events []Event
	Next   string
	// Incomplete prevents receipt retirement when a candidate could not be read.
	Incomplete bool
}

type EventPages func(context.Context, string, string) (EventPage, error)

func SinglePage(source EventSource) EventPages {
	return func(ctx context.Context, owner, _ string) (EventPage, error) {
		events, err := source(ctx, owner)
		return EventPage{Events: events}, err
	}
}
