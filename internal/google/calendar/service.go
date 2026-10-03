// Package calendar supplies Google Calendar operations without exposing OAuth
// tokens to skills. Every call is scoped to a canonical caller and calendar.
package calendar

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/httpbody"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

const (
	scopeCalendarList = "https://www.googleapis.com/auth/calendar.calendarlist.readonly"
	scopeEventsRead   = "https://www.googleapis.com/auth/calendar.events.readonly"
	scopeEventsWrite  = "https://www.googleapis.com/auth/calendar.events"
	apiBase           = "https://www.googleapis.com/calendar/v3"
	responseLimit     = 1 << 20
	requestTimeout    = 30 * time.Second
	stateTTL          = 15 * time.Minute
	maxEvents         = 100
	maxRange          = 366 * 24 * time.Hour
)

type Credentials interface {
	Get(context.Context, string, string) (*memory.PlaintextCredential, error)
	Put(context.Context, *memory.PlaintextCredential) error
	List(context.Context) ([]*memory.PlaintextCredential, error)
	Delete(context.Context, string, string) error
	Issue(context.Context, string, string, memory.TokenRefresher) (*memory.SkillIssue, error)
}
type StateStore interface {
	Get(context.Context, string) (*lobslawv1.IntegrationStateRecord, error)
	Save(context.Context, *lobslawv1.IntegrationStateRecord) error
}
type Config struct {
	ClientID, ClientSecret, RedirectURL string
	Client                              *http.Client
	Credentials                         Credentials
	State                               StateStore
	Authorize                           func(context.Context, string, string) error
}
type Service struct {
	cfg    Config
	client *http.Client
}
type Permission struct {
	Read  bool `json:"read"`
	Write bool `json:"write"`
}
type Connection struct {
	Subject   string                `json:"subject"`
	Email     string                `json:"email"`
	Active    bool                  `json:"active"`
	Calendars map[string]Permission `json:"calendars"`
}
type ConnectionView struct {
	ID        string                `json:"id"`
	Email     string                `json:"email"`
	Calendars map[string]Permission `json:"calendars"`
}
type Query struct {
	Connection string `json:"connection"`
	Calendar   string `json:"calendar"`
	Start      string `json:"start"`
	End        string `json:"end"`
	Event      string `json:"event,omitempty"`
}
type EventTime struct {
	Date     string `json:"date,omitempty"`
	DateTime string `json:"dateTime,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}
type Event struct {
	ID                string            `json:"id"`
	Title             string            `json:"summary"`
	Start             EventTime         `json:"start"`
	End               EventTime         `json:"end"`
	Status            string            `json:"status,omitempty"`
	Link              string            `json:"htmlLink,omitempty"`
	ETag              string            `json:"etag,omitempty"`
	Description       string            `json:"description,omitempty"`
	Location          string            `json:"location,omitempty"`
	RecurringEventID  string            `json:"recurringEventId,omitempty"`
	OriginalStartTime *EventTime        `json:"originalStartTime,omitempty"`
	Attendees         []json.RawMessage `json:"attendees,omitempty"`
	Recurrence        []string          `json:"recurrence,omitempty"`
	EventType         string            `json:"eventType,omitempty"`
}
type EventPage struct {
	Events    []Event `json:"events"`
	Truncated bool    `json:"truncated"`
}
type CalendarView struct {
	ID         string `json:"id"`
	Name       string `json:"summary"`
	TimeZone   string `json:"timeZone"`
	AccessRole string `json:"accessRole"`
}

func New(cfg Config) (*Service, error) {
	u, err := url.Parse(cfg.RedirectURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "/integrations/google/callback" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("calendar: callback must be an HTTPS URL ending /integrations/google/callback")
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.Client == nil || cfg.Credentials == nil || cfg.State == nil || cfg.Authorize == nil {
		return nil, errors.New("calendar: incomplete configuration")
	}
	client := *cfg.Client
	client.Timeout = requestTimeout
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Service{cfg: cfg, client: &client}, nil
}
func principal(ctx context.Context) (string, error) {
	id, ok := turn.IdentityFrom(ctx)
	if !ok || id.Principal == "" || id.Shared {
		return "", errors.New("calendar: authenticated private conversation required")
	}
	return string(id.Principal), nil
}
func (s *Service) authorize(ctx context.Context, action, resource string) error {
	if _, err := principal(ctx); err != nil {
		return err
	}
	return s.cfg.Authorize(ctx, action, resource)
}
func resource(connection, calendar string) string {
	return "google/" + url.PathEscape(connection) + "/" + url.PathEscape(calendar)
}
func decodeConnection(p *memory.PlaintextCredential) (Connection, error) {
	var c Connection
	err := json.Unmarshal(p.ConnectorData, &c)
	return c, err
}
func (s *Service) connection(ctx context.Context, id, cal string, write bool) (*memory.PlaintextCredential, Connection, error) {
	p, err := s.cfg.Credentials.Get(ctx, "google", id)
	if err != nil {
		return nil, Connection{}, errors.New("calendar: connection unavailable")
	}
	c, err := decodeConnection(p)
	if err != nil || !c.Active {
		return nil, c, errors.New("calendar: connection not active")
	}
	permission, ok := c.Calendars[cal]
	action := "calendar:read"
	allowed := permission.Read
	if write {
		action = "calendar:write"
		allowed = permission.Write
	}
	if !ok || !allowed {
		return nil, c, errors.New("calendar: operation not granted for this calendar")
	}
	if err := s.authorize(ctx, action, resource(id, cal)); err != nil {
		return nil, c, err
	}
	return p, c, nil
}
func (s *Service) token(ctx context.Context, id string, write bool) (string, error) {
	issued, err := s.cfg.Credentials.Issue(ctx, "google", id, s.refresh)
	if err != nil {
		return "", err
	}
	if !slices.Contains(issued.Scopes, scopeEventsWrite) && (write || !slices.Contains(issued.Scopes, scopeEventsRead)) {
		return "", errors.New("calendar: Google permission missing; reconnect")
	}
	if !issued.ExpiresAt.After(time.Now()) {
		return "", errors.New("calendar: access token expired; reconnect")
	}
	return issued.AccessToken, nil
}
func (s *Service) request(ctx context.Context, method, target, token string, body any, etag string, out any) error {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		return errors.New("calendar: invalid request")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return errors.New("calendar: request failed; outcome may be uncertain for writes")
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := httpbody.Read(resp.Body, responseLimit)
	if err != nil {
		return errors.New("calendar: response exceeded limit or could not be read")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{Status: resp.StatusCode}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return errors.New("calendar: invalid provider response")
	}
	return nil
}

type HTTPError struct{ Status int }

func (e *HTTPError) Error() string {
	return fmt.Sprintf("calendar: provider returned HTTP %d", e.Status)
}
func eventPath(cal, event string) string {
	p := apiBase + "/calendars/" + url.PathEscape(cal) + "/events"
	if event != "" {
		p += "/" + url.PathEscape(event)
	}
	return p
}
func validID(id string) bool {
	return id != "" && len(id) <= 1024 && id != "." && id != ".." && !strings.ContainsAny(id, "\x00\r\n")
}
func (s *Service) Events(ctx context.Context, q Query) (EventPage, error) {
	var out EventPage
	if !validID(q.Calendar) {
		return out, errors.New("calendar: calendar ID required")
	}
	if _, _, err := s.connection(ctx, q.Connection, q.Calendar, false); err != nil {
		return out, err
	}
	start, err := time.Parse(time.RFC3339, q.Start)
	if err != nil {
		return out, errors.New("calendar: start must be RFC3339")
	}
	end, err := time.Parse(time.RFC3339, q.End)
	if err != nil || !end.After(start) || end.Sub(start) > maxRange {
		return out, errors.New("calendar: invalid date range (maximum 366 days)")
	}
	token, err := s.token(ctx, q.Connection, false)
	if err != nil {
		return out, err
	}
	values := url.Values{"timeMin": {q.Start}, "timeMax": {q.End}, "singleEvents": {"true"}, "orderBy": {"startTime"}, "maxResults": {fmt.Sprint(maxEvents)}, "fields": {"items(id,summary,start,end,status,htmlLink,recurringEventId,originalStartTime),nextPageToken"}}
	var page struct {
		Items         []Event `json:"items"`
		NextPageToken string  `json:"nextPageToken"`
	}
	err = s.request(ctx, http.MethodGet, eventPath(q.Calendar, "")+"?"+values.Encode(), token, nil, "", &page)
	out.Events = page.Items
	out.Truncated = page.NextPageToken != ""
	return out, err
}
func (s *Service) Event(ctx context.Context, q Query) (Event, error) {
	if !validID(q.Calendar) || !validID(q.Event) {
		return Event{}, errors.New("calendar: calendar and event IDs required")
	}
	if _, _, err := s.connection(ctx, q.Connection, q.Calendar, false); err != nil {
		return Event{}, err
	}
	token, err := s.token(ctx, q.Connection, false)
	if err != nil {
		return Event{}, err
	}
	var out Event
	err = s.request(ctx, http.MethodGet, eventPath(q.Calendar, q.Event), token, nil, "", &out)
	return out, err
}
func (s *Service) Calendars(ctx context.Context, id string) ([]CalendarView, error) {
	if err := s.authorize(ctx, "calendar:read", resource(id, "*")); err != nil {
		return nil, err
	}
	p, err := s.cfg.Credentials.Get(ctx, "google", id)
	if err != nil {
		return nil, err
	}
	c, err := decodeConnection(p)
	if err != nil || !c.Active {
		return nil, errors.New("calendar: connection inactive")
	}
	var result []CalendarView
	for id, p := range c.Calendars {
		if p.Read {
			result = append(result, CalendarView{ID: id})
		}
	}
	slices.SortFunc(result, func(a, b CalendarView) int { return strings.Compare(a.ID, b.ID) })
	return result, nil
}
func randomID() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
