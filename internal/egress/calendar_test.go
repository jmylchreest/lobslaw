package egress

import (
	"slices"
	"testing"
)

func TestCalendarEgressOnlyWhenEnabled(t *testing.T) {
	if _, ok := Build(ACLInputs{}).Roles["integration/google-calendar"]; ok {
		t.Fatal("disabled integration has egress")
	}
	hosts := Build(ACLInputs{GoogleCalendar: true}).Roles["integration/google-calendar"]
	if len(hosts) != 3 || !slices.Contains(hosts, "www.googleapis.com") || !slices.Contains(hosts, "oauth2.googleapis.com") || !slices.Contains(hosts, "openidconnect.googleapis.com") {
		t.Fatalf("hosts=%v", hosts)
	}
}
