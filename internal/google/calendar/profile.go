package calendar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type CalendarPreference struct {
	Nickname     string      `json:"nickname,omitempty"`
	Agenda       *bool       `json:"agenda,omitempty"`
	Availability *bool       `json:"availability,omitempty"`
	Permission   *Permission `json:"permission,omitempty"`
}
type Profile struct {
	Calendars    map[string]CalendarPreference `json:"calendars"`
	Defaults     map[string]string             `json:"defaults"`
	TimeZone     string                        `json:"time_zone,omitempty"`
	Disconnected map[string]bool               `json:"disconnected,omitempty"`
	LastChange   string                        `json:"last_change,omitempty"`
}
type CalendarEntry struct {
	ID           string     `json:"id"`
	Connection   string     `json:"connection"`
	Calendar     string     `json:"calendar"`
	Account      string     `json:"account"`
	Nickname     string     `json:"nickname"`
	Permission   Permission `json:"permission"`
	Agenda       bool       `json:"agenda"`
	Availability bool       `json:"availability"`
	Defaults     []string   `json:"default_for,omitempty"`
	Generation   string     `json:"-"`
}
type Inventory struct {
	Revision  uint64          `json:"revision"`
	TimeZone  string          `json:"time_zone,omitempty"`
	Calendars []CalendarEntry `json:"calendars"`
}

func calendarRef(connection, cal string) string {
	sum := sha256.Sum256([]byte(connection + "\x00" + cal))
	return hex.EncodeToString(sum[:12])
}
func normalized(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}
func validNickname(value string) bool {
	return len(value) > 0 && len(value) <= 80 && !strings.ContainsFunc(value, unicode.IsControl)
}
func (s *Service) profile(ctx context.Context) (Profile, *pb.IntegrationStateRecord, error) {
	p := Profile{Calendars: map[string]CalendarPreference{}, Defaults: map[string]string{}, Disconnected: map[string]bool{}}
	if _, err := principal(ctx); err != nil {
		return p, nil, err
	}
	if s.cfg.Settings == nil {
		return p, &pb.IntegrationStateRecord{}, nil
	}
	rec, err := s.cfg.Settings.Get(ctx)
	if err != nil {
		return p, nil, err
	}
	if len(rec.Data) > 0 {
		if err := json.Unmarshal(rec.Data, &p); err != nil {
			return p, nil, errors.New("calendar: settings unavailable")
		}
	}
	if p.Calendars == nil {
		p.Calendars = map[string]CalendarPreference{}
	}
	if p.Defaults == nil {
		p.Defaults = map[string]string{}
	}
	if p.Disconnected == nil {
		p.Disconnected = map[string]bool{}
	}
	return p, rec, nil
}
func (s *Service) Inventory(ctx context.Context) (Inventory, error) {
	if err := s.authorize(ctx, "calendar:connect", "google"); err != nil {
		return Inventory{}, err
	}
	p, r, err := s.profile(ctx)
	if err != nil {
		return Inventory{}, err
	}
	return s.inventory(ctx, p, r.Revision)
}
func (s *Service) inventory(ctx context.Context, p Profile, revision uint64) (Inventory, error) {
	out := Inventory{Revision: revision, TimeZone: p.TimeZone, Calendars: []CalendarEntry{}}
	rows, err := s.cfg.Credentials.List(ctx)
	if err != nil {
		return out, err
	}
	for _, row := range rows {
		if row.Provider != "google" || p.Disconnected[row.Subject] {
			continue
		}
		c, err := decodeConnection(row)
		if err != nil {
			return out, err
		}
		if !c.Active {
			continue
		}
		for cal, permission := range c.Calendars {
			ref := calendarRef(row.Subject, cal)
			pref := p.Calendars[ref]
			name := pref.Nickname
			if name == "" {
				base := c.Names[cal]
				if !validNickname(base) {
					base = "Calendar"
				}
				name = fmt.Sprintf("%s (%s)", base, ref[:6])
			}
			if pref.Permission != nil {
				permission = *pref.Permission
			}
			item := CalendarEntry{ID: ref, Connection: row.Subject, Calendar: cal, Account: c.Email, Nickname: name, Permission: permission, Agenda: true, Availability: true, Generation: row.Generation}
			if pref.Agenda != nil {
				item.Agenda = *pref.Agenda
			}
			if pref.Availability != nil {
				item.Availability = *pref.Availability
			}
			for purpose, target := range p.Defaults {
				if target == ref {
					item.Defaults = append(item.Defaults, purpose)
				}
			}
			slices.Sort(item.Defaults)
			out.Calendars = append(out.Calendars, item)
		}
	}
	slices.SortFunc(out.Calendars, func(a, b CalendarEntry) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}
func selectCalendar(in Inventory, selector string) (CalendarEntry, error) {
	var selected []CalendarEntry
	for _, c := range in.Calendars {
		if c.ID == selector || normalized(c.Nickname) == normalized(selector) {
			selected = append(selected, c)
		}
	}
	if len(selected) != 1 {
		return CalendarEntry{}, errors.New("calendar: calendar name is missing or ambiguous; ask the user to choose from calendar_settings")
	}
	return selected[0], nil
}

// Resolve converts a nickname or explicit purpose default to one owned target.
// Raw IDs remain supported for older tools; resolution never grants access.
func (s *Service) Resolve(ctx context.Context, connection, cal, purpose string, write bool) (CalendarEntry, error) {
	p, r, err := s.profile(ctx)
	if err != nil {
		return CalendarEntry{}, err
	}
	in, err := s.inventory(ctx, p, r.Revision)
	if err != nil {
		return CalendarEntry{}, err
	}
	var target CalendarEntry
	if connection != "" {
		for _, c := range in.Calendars {
			if c.Connection == connection && c.Calendar == cal {
				target = c
				break
			}
		}
		if target.ID == "" {
			return target, errors.New("calendar: selected calendar unavailable")
		}
	} else {
		if cal == "" {
			if purpose == "" {
				purpose = "general"
			}
			cal = p.Defaults[normalized(purpose)]
			if cal == "" {
				return target, errors.New("calendar: no default for this purpose; ask which calendar to use")
			}
		}
		target, err = selectCalendar(in, cal)
		if err != nil {
			return target, err
		}
	}
	if _, _, err := s.connection(ctx, target.Connection, target.Calendar, write); err != nil {
		return CalendarEntry{}, err
	}
	return target, nil
}
