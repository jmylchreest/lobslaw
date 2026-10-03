package calendar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type Mutation struct {
	Connection  string    `json:"connection"`
	Calendar    string    `json:"calendar"`
	Event       string    `json:"event,omitempty"`
	Title       string    `json:"title"`
	Start       EventTime `json:"start"`
	End         EventTime `json:"end"`
	Description *string   `json:"description,omitempty"`
	Location    *string   `json:"location,omitempty"`
}
type Preview struct {
	ApprovalKey string `json:"approval_key"`
	ID          string `json:"id"`
	Summary     string `json:"summary"`
}
type pendingMutation struct {
	Nonce      string   `json:"nonce"`
	Operation  string   `json:"operation"`
	Mutation   Mutation `json:"mutation"`
	Generation string   `json:"generation"`
	ETag       string   `json:"etag"`
	Status     string   `json:"status"`
	Result     *Event   `json:"result,omitempty"`
	Before     *Event   `json:"before,omitempty"`
}

func eventInstant(e EventTime) (time.Time, error) {
	if e.TimeZone != "" {
		if _, err := time.LoadLocation(e.TimeZone); err != nil {
			return time.Time{}, errors.New("calendar: invalid IANA time zone")
		}
	}
	if (e.Date == "") == (e.DateTime == "") {
		return time.Time{}, errors.New("calendar: specify either date or dateTime")
	}
	if e.Date != "" {
		return time.Parse(time.DateOnly, e.Date)
	}
	return time.Parse(time.RFC3339, e.DateTime)
}
func validateMutation(operation string, m Mutation) error {
	if operation != "create" && operation != "update" {
		return errors.New("calendar: unsupported mutation")
	}
	if !validID(m.Calendar) || m.Title == "" || len(m.Title) > 1024 {
		return errors.New("calendar: calendar and bounded title required")
	}
	if operation == "update" && !validID(m.Event) {
		return errors.New("calendar: event ID required for update")
	}
	if operation == "create" && m.Event != "" {
		return errors.New("calendar: caller cannot select created event ID")
	}
	start, err := eventInstant(m.Start)
	if err != nil {
		return err
	}
	end, err := eventInstant(m.End)
	if err != nil {
		return err
	}
	if !end.After(start) || (m.Start.Date == "") != (m.End.Date == "") {
		return errors.New("calendar: invalid event interval")
	}
	if (m.Description != nil && len(*m.Description) > 16<<10) || (m.Location != nil && len(*m.Location) > 2048) {
		return errors.New("calendar: event field too large")
	}
	return nil
}
func (s *Service) Prepare(ctx context.Context, operation string, m Mutation) (Preview, error) {
	var preview Preview
	if err := validateMutation(operation, m); err != nil {
		return preview, err
	}
	p, _, err := s.connection(ctx, m.Connection, m.Calendar, true)
	if err != nil {
		return preview, err
	}
	if _, err := s.token(ctx, m.Connection, true); err != nil {
		return preview, err
	}
	id, _ := turn.IdentityFrom(ctx)
	if id.TurnID == "" {
		return preview, errors.New("calendar: mutation requires a turn identity")
	}
	encoded, _ := json.Marshal(struct {
		Owner, Turn, Operation string
		Mutation               Mutation
	}{string(id.Principal), id.TurnID, operation, m})
	digest := sha256.Sum256(encoded)
	key := hex.EncodeToString(digest[:])
	if _, existing, err := s.pending(ctx, key); err == nil {
		return mutationPreview(key, existing), nil
	}
	pending := pendingMutation{Nonce: randomID(), Operation: operation, Mutation: m, Generation: p.Generation, Status: "pending"}
	if operation == "update" {
		before, err := s.Event(ctx, Query{Connection: m.Connection, Calendar: m.Calendar, Event: m.Event})
		if err != nil {
			return preview, err
		}
		if err := writableEvent(before); err != nil {
			return preview, err
		}
		if before.ETag == "" {
			return preview, errors.New("calendar: provider omitted event version")
		}
		pending.ETag = before.ETag
		pending.Before = &Event{ID: before.ID, Title: before.Title, Start: before.Start, End: before.End, ETag: before.ETag}
		if m.Description != nil {
			pending.Before.Description = before.Description
		}
		if m.Location != nil {
			pending.Before.Location = before.Location
		}
	}
	if len(mutationPreview(key, pending).Summary) > 2800 {
		return preview, errors.New("calendar: change is too large to review safely; shorten the fields or edit in Google Calendar")
	}
	data, _ := json.Marshal(pending)
	rec := &lobslawv1.IntegrationStateRecord{Id: key, Owner: string(id.Principal), Kind: "calendar-mutation", Data: data, ExpiresAt: timestamppb.New(time.Now().Add(stateTTL))}
	if err := s.cfg.State.Save(ctx, rec); err != nil {
		return preview, err
	}
	return mutationPreview(key, pending), nil
}
func writableEvent(e Event) error {
	if len(e.Attendees) > 0 || len(e.Recurrence) > 0 || e.Status == "cancelled" || (e.EventType != "" && e.EventType != "default") {
		return errors.New("calendar: guest events, series edits, cancellations and special events are not supported")
	}
	return nil
}
func mutationPreview(id string, p pendingMutation) Preview {
	data, _ := json.MarshalIndent(p.Mutation, "", "  ")
	summary := fmt.Sprintf("%s Google Calendar event (write + network). Exact change:\n%s", p.Operation, data)
	if p.Before != nil {
		before, _ := json.MarshalIndent(p.Before, "", "  ")
		summary += "\nCurrent event:\n" + string(before)
	}
	return Preview{ID: id, ApprovalKey: id + "/" + p.Nonce, Summary: summary}
}
func (s *Service) pending(ctx context.Context, id string) (*lobslawv1.IntegrationStateRecord, pendingMutation, error) {
	var p pendingMutation
	owner, err := principal(ctx)
	if err != nil {
		return nil, p, err
	}
	r, err := s.cfg.State.Get(ctx, id)
	if err != nil {
		return nil, p, err
	}
	if r.Owner != owner || r.Kind != "calendar-mutation" || r.ExpiresAt == nil || !r.ExpiresAt.AsTime().After(time.Now()) {
		return nil, p, errors.New("calendar: mutation unavailable")
	}
	err = json.Unmarshal(r.Data, &p)
	return r, p, err
}
func (s *Service) savePending(ctx context.Context, r *lobslawv1.IntegrationStateRecord, p pendingMutation) error {
	r.Data, _ = json.Marshal(p)
	return s.cfg.State.Save(ctx, r)
}

// Approve is called only by the trusted executor gate after exact human approval.
// It is intentionally not exposed as an agent tool.
func (s *Service) Approve(ctx context.Context, id string) error {
	r, p, err := s.pending(ctx, id)
	if err != nil {
		return err
	}
	if _, _, err := s.connection(ctx, p.Mutation.Connection, p.Mutation.Calendar, true); err != nil {
		return err
	}
	if p.Status == "approved" || p.Status == "done" {
		return nil
	}
	if p.Status != "pending" {
		return errors.New("calendar: mutation already dispatched; inspect the event")
	}
	p.Status = "approved"
	return s.savePending(ctx, r, p)
}
func (s *Service) Apply(ctx context.Context, id string) (Event, error) {
	r, p, err := s.pending(ctx, id)
	if err != nil {
		return Event{}, err
	}
	credential, _, err := s.connection(ctx, p.Mutation.Connection, p.Mutation.Calendar, true)
	if err != nil {
		return Event{}, err
	}
	if credential.Generation != p.Generation {
		return Event{}, errors.New("calendar: connection changed; prepare a new change")
	}
	if p.Status == "done" && p.Result != nil {
		return *p.Result, nil
	}
	if p.Status != "approved" {
		return Event{}, errors.New("calendar: exact approval required or prior outcome uncertain; inspect the event")
	}
	token, err := s.token(ctx, p.Mutation.Connection, true)
	if err != nil {
		return Event{}, err
	}
	m := p.Mutation
	if p.Operation == "update" {
		current, err := s.Event(ctx, Query{Connection: m.Connection, Calendar: m.Calendar, Event: m.Event})
		if err != nil {
			return Event{}, err
		}
		if err := writableEvent(current); err != nil {
			return Event{}, err
		}
		if current.ETag != p.ETag {
			return Event{}, errors.New("calendar: event changed since review; start a new turn to review it again")
		}
	}
	// Fence duplicate resumes BEFORE transmitting a write. A crash or timeout
	// leaves dispatched state; no automatic second write can follow it.
	p.Status = "dispatched"
	if err := s.savePending(ctx, r, p); err != nil {
		return Event{}, err
	}
	r.Revision++
	body := map[string]any{"summary": m.Title, "start": m.Start, "end": m.End}
	if m.Description != nil {
		body["description"] = *m.Description
	}
	if m.Location != nil {
		body["location"] = *m.Location
	}
	method := http.MethodPatch
	eventID := m.Event
	if p.Operation == "create" {
		method = http.MethodPost
		body["id"] = id
		body["guestsCanInviteOthers"] = false
	}
	var event Event
	err = s.request(ctx, method, eventPath(m.Calendar, eventID)+"?sendUpdates=none", token, body, p.ETag, &event)
	if err != nil {
		return Event{}, fmt.Errorf("%w; operation %s is not retried automatically", err, id)
	}
	p.Status = "done"
	p.Result = &event
	if err := s.savePending(ctx, r, p); err != nil {
		return Event{}, errors.New("calendar: Google accepted the change but receipt persistence failed; inspect the event before retrying")
	}
	return event, nil
}
