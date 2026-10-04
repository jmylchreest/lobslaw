package calendar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type SettingsChange struct {
	Operation  string      `json:"operation"`
	Calendar   string      `json:"calendar,omitempty"`
	Value      string      `json:"value,omitempty"`
	Enabled    *bool       `json:"enabled,omitempty"`
	Permission *Permission `json:"permission,omitempty"`
	Revision   *uint64     `json:"revision,omitempty"`
}
type SettingsPreview struct {
	ID          string `json:"id"`
	ApprovalKey string `json:"approval_key"`
	Summary     string `json:"summary"`
	Confirm     bool   `json:"confirm"`
}
type SettingsResult struct {
	Inventory Inventory `json:"settings"`
	Undo      string    `json:"undo,omitempty"`
}
type settingsPending struct {
	Change     SettingsChange `json:"change"`
	Before     Profile        `json:"before"`
	After      Profile        `json:"after"`
	Revision   uint64         `json:"revision"`
	Target     CalendarEntry  `json:"target"`
	Generation string         `json:"generation"`
	Status     string         `json:"status"`
	Nonce      string         `json:"nonce"`
	Confirm    bool           `json:"confirm"`
	Summary    string         `json:"summary"`
}

func (s *Service) loadSettingsChange(ctx context.Context, id string) (*pb.IntegrationStateRecord, settingsPending, error) {
	var p settingsPending
	owner, err := principal(ctx)
	if err != nil {
		return nil, p, err
	}
	r, err := s.cfg.State.Get(ctx, id)
	if err != nil {
		return nil, p, err
	}
	if r.Kind != "calendar-settings-change" || r.Owner != owner || r.ExpiresAt == nil || !r.ExpiresAt.AsTime().After(time.Now()) {
		return nil, p, errors.New("calendar: settings change unavailable")
	}
	err = json.Unmarshal(r.Data, &p)
	return r, p, err
}
func settingsPreview(id string, p settingsPending) SettingsPreview {
	return SettingsPreview{ID: id, ApprovalKey: id + "/" + p.Nonce, Summary: p.Summary, Confirm: p.Confirm}
}
func (s *Service) PrepareSettings(ctx context.Context, change SettingsChange) (SettingsPreview, error) {
	if err := s.authorize(ctx, "calendar:connect", "google"); err != nil {
		return SettingsPreview{}, err
	}
	if s.cfg.Settings == nil {
		return SettingsPreview{}, errors.New("calendar: settings storage unavailable")
	}
	who, _ := turn.IdentityFrom(ctx)
	if who.TurnID == "" {
		return SettingsPreview{}, errors.New("calendar: settings change requires a turn")
	}
	raw, _ := json.Marshal(struct {
		Owner, Turn string
		Change      SettingsChange
	}{string(who.Principal), who.TurnID, change})
	sum := sha256.Sum256(raw)
	id := hex.EncodeToString(sum[:])
	if _, old, err := s.loadSettingsChange(ctx, id); err == nil {
		return settingsPreview(id, old), nil
	}
	before, rec, err := s.profile(ctx)
	if err != nil {
		return SettingsPreview{}, err
	}
	if change.Revision != nil && *change.Revision != rec.Revision {
		return SettingsPreview{}, errors.New("calendar: settings changed; reopen /calendars")
	}
	raw, _ = json.Marshal(before)
	var after Profile
	_ = json.Unmarshal(raw, &after)
	in, err := s.inventory(ctx, before, rec.Revision)
	if err != nil {
		return SettingsPreview{}, err
	}
	p := settingsPending{Change: change, Before: before, After: after, Revision: rec.Revision, Status: "pending", Nonce: randomID(), Confirm: true}
	if change.Operation != "timezone" && change.Operation != "undo" {
		p.Target, err = selectCalendar(in, change.Calendar)
		if err != nil {
			return SettingsPreview{}, err
		}
		p.Generation = p.Target.Generation
	}
	after, err = s.applySettingsChange(ctx, change, before, after, rec, in, &p)
	if err != nil {
		return SettingsPreview{}, err
	}
	if len(after.Calendars) > 1000 || len(after.Defaults) > 100 {
		return SettingsPreview{}, errors.New("calendar: settings limit reached")
	}
	after.LastChange = id
	p.After = after
	details, _ := json.MarshalIndent(change, "", "  ")
	p.Summary = fmt.Sprintf("Calendar settings: %s\n%s", change.Operation, details)
	if p.Target.ID != "" {
		p.Summary += fmt.Sprintf("\nCalendar: %s\nAccount: %s\nProvider calendar: %s", p.Target.Nickname, p.Target.Account, p.Target.Calendar)
	}
	if change.Operation == "disconnect" {
		p.Summary += "\nDisconnect this Google account connection and ALL its selected calendars."
	}
	if len(p.Summary) > 2800 {
		return SettingsPreview{}, errors.New("calendar: settings preview too large")
	}
	if !p.Confirm {
		p.Status = "approved"
	}
	data, _ := json.Marshal(p)
	r := &pb.IntegrationStateRecord{Id: id, Owner: string(who.Principal), Kind: "calendar-settings-change", Data: data, ExpiresAt: timestamppb.New(time.Now().Add(stateTTL))}
	if err := s.cfg.State.Save(ctx, r); err != nil {
		return SettingsPreview{}, err
	}
	return settingsPreview(id, p), nil
}
func (s *Service) validateAccess(ctx context.Context, target CalendarEntry, want Permission) error {
	// Narrowing requires no provider request. Expansion checks operator policy,
	// Google scopes and the provider calendar's current access role.
	if (!want.Read || target.Permission.Read) && (!want.Write || target.Permission.Write) {
		return nil
	}
	if want.Read {
		if err := s.authorize(ctx, "calendar:read", resource(target.Connection, target.Calendar)); err != nil {
			return err
		}
	}
	if want.Write {
		if err := s.authorize(ctx, "calendar:write", resource(target.Connection, target.Calendar)); err != nil {
			return err
		}
	}
	token, err := s.token(ctx, target.Connection, want.Write)
	if err != nil {
		return err
	}
	var cal CalendarView
	if err := s.request(ctx, http.MethodGet, apiBase+"/users/me/calendarList/"+url.PathEscape(target.Calendar), token, nil, "", &cal); err != nil {
		return err
	}
	if want.Read && cal.AccessRole != "owner" && cal.AccessRole != "writer" && cal.AccessRole != "reader" {
		return errors.New("calendar: Google does not allow event reads from this calendar")
	}
	if want.Write && cal.AccessRole != "owner" && cal.AccessRole != "writer" {
		return errors.New("calendar: Google does not allow writes to this calendar")
	}
	return nil
}
func (s *Service) ApproveSettings(ctx context.Context, id string) error {
	if err := s.authorize(ctx, "calendar:connect", "google"); err != nil {
		return err
	}
	r, p, err := s.loadSettingsChange(ctx, id)
	if err != nil {
		return err
	}
	if p.Status == "approved" || p.Status == "done" {
		return nil
	}
	if p.Status != "pending" {
		return errors.New("calendar: invalid settings approval")
	}
	p.Status = "approved"
	r.Data, _ = json.Marshal(p)
	return s.cfg.State.Save(ctx, r)
}
func (s *Service) ApplySettings(ctx context.Context, id string) (SettingsResult, error) {
	if err := s.authorize(ctx, "calendar:connect", "google"); err != nil {
		return SettingsResult{}, err
	}
	r, p, err := s.loadSettingsChange(ctx, id)
	if err != nil {
		return SettingsResult{}, err
	}
	if p.Status != "approved" && p.Status != "done" {
		return SettingsResult{}, errors.New("calendar: exact settings approval required")
	}
	current, rec, err := s.profile(ctx)
	if err != nil {
		return SettingsResult{}, err
	}
	if current.LastChange != id {
		if rec.Revision != p.Revision {
			return SettingsResult{}, errors.New("calendar: settings changed since review; prepare again")
		}
		if p.Target.ID != "" {
			cred, err := s.cfg.Credentials.Get(ctx, "google", p.Target.Connection)
			if err != nil || cred.Generation != p.Generation {
				return SettingsResult{}, errors.New("calendar: account changed since review")
			}
		}
		if p.Change.Operation == "access" {
			if err := s.validateAccess(ctx, p.Target, *p.Change.Permission); err != nil {
				return SettingsResult{}, err
			}
		}
		rec.Data, _ = json.Marshal(p.After)
		if err := s.cfg.Settings.Save(ctx, rec); err != nil {
			return SettingsResult{}, err
		}
	}
	if p.Change.Operation == "disconnect" && p.Status != "done" {
		if err := s.cfg.Credentials.Delete(ctx, "google", p.Target.Connection); err != nil && !memory.IsCredentialNotFound(err) {
			return SettingsResult{}, errors.New("calendar: access blocked but credential cleanup failed; disconnect again")
		}
	}
	if p.Status != "done" {
		p.Status = "done"
		r.Data, _ = json.Marshal(p)
		if err := s.cfg.State.Save(ctx, r); err != nil {
			return SettingsResult{}, err
		}
	}
	inventory, err := s.Inventory(ctx)
	out := SettingsResult{Inventory: inventory}
	if !p.Confirm && p.Change.Operation != "undo" {
		out.Undo = id
	}
	return out, err
}

func (s *Service) applySettingsChange(ctx context.Context, change SettingsChange, before, after Profile, rec *pb.IntegrationStateRecord, in Inventory, p *settingsPending) (Profile, error) {
	pref := after.Calendars[p.Target.ID]
	switch change.Operation {
	case "rename":
		name, err := calendarNickname(in, p.Target.ID, change.Value)
		if err != nil {
			return after, err
		}
		pref.Nickname = name
		p.Confirm = false
	case "agenda", "availability":
		if change.Enabled == nil {
			return after, errors.New("calendar: enabled is required")
		}
		if change.Operation == "agenda" {
			pref.Agenda = change.Enabled
			p.Confirm = false
		} else {
			pref.Availability = change.Enabled
		}
	case "default":
		purpose := normalized(change.Value)
		if !validNickname(purpose) {
			return after, errors.New("calendar: name a purpose such as general, personal or family")
		}
		if change.Enabled != nil && !*change.Enabled {
			delete(after.Defaults, purpose)
		} else {
			if !p.Target.Permission.Write {
				return after, errors.New("calendar: default destination must be writable")
			}
			after.Defaults[purpose] = p.Target.ID
		}
	case "timezone":
		if _, err := time.LoadLocation(change.Value); err != nil || change.Value == "" {
			return after, errors.New("calendar: valid IANA time zone required")
		}
		after.TimeZone = change.Value
	case "access":
		if change.Permission == nil {
			return after, errors.New("calendar: read/write flags required")
		}
		if err := s.validateAccess(ctx, p.Target, *change.Permission); err != nil {
			return after, err
		}
		pref.Permission = change.Permission
		if !change.Permission.Write {
			clearCalendarDefaults(&after, p.Target.ID)
		}
	case "disconnect":
		after.Disconnected[p.Target.Connection] = true
		for _, c := range in.Calendars {
			if c.Connection == p.Target.Connection {
				delete(after.Calendars, c.ID)
				clearCalendarDefaults(&after, c.ID)
			}
		}
	case "undo":
		_, previous, err := s.loadSettingsChange(ctx, change.Value)
		if err != nil {
			return after, err
		}
		if previous.Confirm || previous.Status != "done" || before.LastChange != change.Value || rec.Revision != previous.Revision+1 {
			return after, errors.New("calendar: undo no longer available after another settings change")
		}
		after = previous.Before
		p.Confirm = false
	default:
		return after, errors.New("calendar: unsupported settings operation")
	}
	if p.Target.ID != "" && change.Operation != "disconnect" {
		after.Calendars[p.Target.ID] = pref
	}
	return after, nil
}

func calendarNickname(in Inventory, id, value string) (string, error) {
	name := strings.Join(strings.Fields(value), " ")
	if !validNickname(name) {
		return "", errors.New("calendar: nickname must be 1–80 printable characters")
	}
	for _, c := range in.Calendars {
		if c.ID != id && (normalized(c.Nickname) == normalized(name) || c.ID == name) {
			return "", errors.New("calendar: nickname already in use")
		}
	}
	return name, nil
}

func clearCalendarDefaults(p *Profile, id string) {
	for purpose, target := range p.Defaults {
		if target == id {
			delete(p.Defaults, purpose)
		}
	}
}
