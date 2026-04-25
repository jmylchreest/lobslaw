package compute

import (
	"testing"
	"time"
)

func TestParseWhenAcceptsDuration(t *testing.T) {
	t.Parallel()
	got, err := parseWhen("2m")
	if err != nil {
		t.Fatal(err)
	}
	delta := time.Until(got)
	if delta < time.Minute || delta > 3*time.Minute {
		t.Errorf("expected ~2 minutes ahead; got %s", delta)
	}
}

func TestParseWhenAcceptsRFC3339(t *testing.T) {
	t.Parallel()
	got, err := parseWhen("2030-01-01T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if got.Year() != 2030 {
		t.Errorf("year=%d", got.Year())
	}
}

func TestParseWhenRejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, err := parseWhen("not a time"); err == nil {
		t.Error("garbage input should fail")
	}
}

func TestParseWhenRejectsEmpty(t *testing.T) {
	t.Parallel()
	if _, err := parseWhen(""); err == nil {
		t.Error("empty input should fail")
	}
}
