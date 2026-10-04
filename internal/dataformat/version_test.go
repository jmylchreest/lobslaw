package dataformat

import (
	"context"
	"errors"
	"os"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func TestUpgradeRejectsGapsAndFuture(t *testing.T) {
	for _, version := range []int{-1, 0, 3} {
		if _, err := Upgrade(context.Background(), "original", version, 2, []Step[string]{}); err == nil {
			t.Fatalf("accepted version %d", version)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Upgrade(ctx, "", 1, 1, []Step[string]{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestLegacyLogCollision(t *testing.T) {
	// An ID-only DELETE is valid under both historical schemas: do not guess.
	raw := protowire.AppendTag(nil, 38, protowire.BytesType)
	raw = protowire.AppendBytes(raw, []byte{10, 1, 'x'})
	before := append([]byte(nil), raw...)
	if _, err := NormalizeLog(raw, ""); !errors.Is(err, ErrAmbiguous) {
		t.Fatal(err)
	}
	teams, err := NormalizeLog(raw, LegacyTeams)
	if err != nil {
		t.Fatal(err)
	}
	number, _, _ := protowire.ConsumeTag(teams)
	if number != 53 {
		t.Fatal(number)
	}
	main, err := NormalizeLog(raw, LegacyMain)
	if err != nil {
		t.Fatal(err)
	}
	number, _, _ = protowire.ConsumeTag(main)
	if number != 38 {
		t.Fatal(number)
	}
	again, err := NormalizeLog(teams, LegacyMain)
	if err != nil || string(again) != string(teams) {
		t.Fatal("not idempotent", err)
	}
	if string(raw) != string(before) {
		t.Fatal("mutated source")
	}
}

func TestMalformedAndFutureLog(t *testing.T) {
	future := protowire.AppendTag(nil, LogVersionField, protowire.VarintType)
	future = protowire.AppendVarint(future, LogVersion+1)
	duplicate := append(append([]byte(nil), future...), future...)
	for _, raw := range [][]byte{{255}, future, duplicate} {
		if _, err := NormalizeLog(raw, LegacyMain); err == nil {
			t.Fatal("accepted invalid log")
		}
	}
}

func TestHistoricalSchemaFixtures(t *testing.T) {
	for _, test := range []struct {
		profile string
		field   protowire.Number
	}{{LegacyMain, 39}, {LegacyTeams, 53}, {LegacyTeamsEarly, 51}} {
		t.Run(test.profile, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/" + test.profile + ".binpb")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NormalizeLog(raw, ""); !errors.Is(err, ErrAmbiguous) {
				t.Fatalf("legacy collision guessed: %v", err)
			}
			normalized, err := NormalizeLog(raw, test.profile)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for len(normalized) > 0 {
				number, typ, n := protowire.ConsumeTag(normalized)
				size := protowire.ConsumeFieldValue(number, typ, normalized[n:])
				if number == test.field {
					found = true
				}
				normalized = normalized[n+size:]
			}
			if !found {
				t.Fatalf("missing translated field %d", test.field)
			}
		})
	}
}
