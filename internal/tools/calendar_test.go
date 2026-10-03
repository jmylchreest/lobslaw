package tools

import (
	"testing"

	"github.com/jmylchreest/lobslaw/internal/google/calendar"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestCalendarArgumentsKeepTimesAndRejectArbitraryAPIFields(t *testing.T) {
	var q calendar.Query
	if err := calendarArguments(map[string]string{"connection": "a", "calendar": "b", "start": "2026-10-03T10:00:00Z", "end": "2026-10-03T11:00:00Z"}, &q); err != nil {
		t.Fatal(err)
	}
	m, err := CalendarMutation(map[string]string{"connection": "a", "calendar": "b", "title": "visit", "start": `{"date":"2026-10-03"}`, "end": `{"date":"2026-10-04"}`})
	if err != nil || m.Start.Date != "2026-10-03" {
		t.Fatalf("%+v %v", m, err)
	}
	for _, args := range []map[string]string{{"attendees": "[]"}, {"sendUpdates": "all"}, {"__user_id": "bob"}, {"start": `{"date":"2026-10-03","hidden":"x"}`}} {
		if _, err := CalendarMutation(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
func TestCalendarEffectsAreTrustedAndCloned(t *testing.T) {
	r := NewRegistry()
	for _, def := range CalendarToolDefs() {
		if err := r.Register(def); err != nil {
			t.Fatal(err)
		}
		def.Effects.State = types.ToolDeletes
	}
	for _, def := range r.List() {
		if def.Effects == nil || (def.Name != "calendar_list" && !def.Effects.Network) || def.Effects.State == types.ToolDeletes {
			t.Fatalf("bad effects: %+v", def)
		}
	}
	if !calendarEventUpdateDef().Effects.Reads {
		t.Fatal("update lost preview read")
	}
}
