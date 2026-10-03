package main

import "testing"

func TestCalendarToolClassification(t *testing.T) {
	for _, name := range []string{"calendar_list", "calendar_events", "calendar_event", "calendar_event_create", "calendar_event_update"} {
		if err := policyClassify([]string{"--tool", name, "--json"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := policyClassify([]string{"--tool", "calendar_delete"}); err == nil {
		t.Fatal("unknown tool classified")
	}
	if err := policyClassify([]string{"--tool", "calendar_events", "rm -rf /"}); err == nil {
		t.Fatal("mixed invocation accepted")
	}
}
