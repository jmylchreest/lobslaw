package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
)

type CalendarEvents struct {
	Calendar  CalendarEntry `json:"calendar"`
	Events    []Event       `json:"events"`
	Error     string        `json:"error,omitempty"`
	Truncated bool          `json:"truncated"`
}
type AgendaResult struct {
	Calendars []CalendarEvents `json:"calendars"`
	Complete  bool             `json:"complete"`
	TimeZone  string           `json:"time_zone,omitempty"`
	Warning   string           `json:"warning,omitempty"`
}

// Agenda retains source identities and failures; an incomplete result never proves availability.
func (s *Service) Agenda(ctx context.Context, start, end, mode string) (AgendaResult, error) {
	out := AgendaResult{Complete: true, Calendars: []CalendarEvents{}}
	if mode == "" {
		mode = "agenda"
	}
	if mode != "agenda" && mode != "availability" {
		return out, errors.New("calendar: mode must be agenda or availability")
	}
	a, err := time.Parse(time.RFC3339, start)
	if err != nil {
		return out, err
	}
	b, err := time.Parse(time.RFC3339, end)
	if err != nil || !b.After(a) || b.Sub(a) > maxRange {
		return out, errors.New("calendar: invalid date range")
	}
	p, r, err := s.profile(ctx)
	if err != nil {
		return out, err
	}
	in, err := s.inventory(ctx, p, r.Revision)
	if err != nil {
		return out, err
	}
	out.TimeZone = in.TimeZone
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for _, c := range in.Calendars {
		if (mode == "agenda" && !c.Agenda) || (mode == "availability" && !c.Availability) {
			continue
		}
		item := CalendarEvents{Calendar: c}
		if len(out.Calendars) >= 25 {
			out.Complete = false
			out.Warning = "Calendar limit reached; results incomplete."
			break
		}
		page, err := s.Events(ctx, Query{Connection: c.Connection, Calendar: c.Calendar, Start: start, End: end})
		if err != nil {
			item.Error = err.Error()
			out.Complete = false
		} else {
			item.Events = page.Events
			if mode == "availability" {
				item.Events = slices.DeleteFunc(item.Events, nonBlockingEvent)
			}
			item.Truncated = page.Truncated
			if page.Truncated {
				out.Complete = false
			}
		}
		slices.SortFunc(item.Events, func(a, b Event) int {
			return strings.Compare(a.Start.Date+a.Start.DateTime, b.Start.Date+b.Start.DateTime)
		})
		out.Calendars = append(out.Calendars, item)
	}
	if len(out.Calendars) == 0 {
		out.Complete = false
		out.Warning = "No calendars selected for this view."
	}
	if mode == "availability" {
		out.Warning = "These are events from availability calendars, not a free/busy guarantee. Do not assert availability when complete=false. Consider all-day events and time zones."
	}
	return out, nil
}

func nonBlockingEvent(e Event) bool {
	if e.Status == "cancelled" || e.Transparency == "transparent" {
		return true
	}
	for _, raw := range e.Attendees {
		var attendee struct {
			Self     bool   `json:"self"`
			Response string `json:"responseStatus"`
		}
		if json.Unmarshal(raw, &attendee) == nil && attendee.Self && attendee.Response == "declined" {
			return true
		}
	}
	return false
}
