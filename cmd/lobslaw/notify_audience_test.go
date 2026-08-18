package main

import (
	"reflect"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

func telegramOwner() []config.GatewayChannelConfig {
	return []config.GatewayChannelConfig{{
		Type:       "telegram",
		UserScopes: map[string]string{"6972251926": "owner", "111": "public"},
	}}
}

// THE POINT. mode = "propose" is the statement that a human should
// look before anything takes effect. Requiring a second block to hear
// about the queue made it mean "write to a queue nobody is told
// about" — auto mode with extra steps, and worse, because proposal
// expiry then discards things nobody declined.
func TestProposeModeNudgesWithoutBeingAskedTwice(t *testing.T) {
	t.Parallel()
	got := resolveNoticeAudience("propose", config.NotifyConfig{}, telegramOwner())
	if !got.Enabled {
		t.Fatal("propose mode resolved to silence")
	}
	if !reflect.DeepEqual(got.Channels, []string{"telegram"}) {
		t.Errorf("channels = %v, want the configured gateway channel", got.Channels)
	}
	// Both forms: a Telegram turn is attributed under the tg- prefixed
	// principal while the scope map is keyed by the bare numeric id.
	// That mismatch already made a confirmation unattributable once.
	want := []string{"user:6972251926", "user:tg-6972251926"}
	if !reflect.DeepEqual(got.Subjects, want) {
		t.Errorf("subjects = %v, want %v", got.Subjects, want)
	}
}

// Owner only. A public user cannot approve an artefact, so telling
// them one is waiting is noise about a door they cannot open.
func TestOnlyOwnersAreTold(t *testing.T) {
	t.Parallel()
	got := resolveNoticeAudience("propose", config.NotifyConfig{}, telegramOwner())
	for _, s := range got.Subjects {
		if s == "111" || s == "tg-111" {
			t.Errorf("a public-scoped user is in the audience: %v", got.Subjects)
		}
	}
}

// The other modes have no queue: auto applies artefacts immediately
// and off writes none, so there is nothing waiting on a person.
func TestOnlyProposeModeNudges(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"auto", "off", ""} {
		if resolveNoticeAudience(mode, config.NotifyConfig{}, telegramOwner()).Enabled {
			t.Errorf("mode %q produced a review nudge", mode)
		}
	}
}

func TestTheNudgeCanBeTurnedOff(t *testing.T) {
	t.Parallel()
	if resolveNoticeAudience("propose", config.NotifyConfig{Disabled: true}, telegramOwner()).Enabled {
		t.Error("notify.disabled did not silence propose mode")
	}
}

// An operator who named a channel or a subject meant that list.
// Quietly widening it would send a notice somewhere they had decided
// against.
func TestExplicitListsAreNotWidened(t *testing.T) {
	t.Parallel()
	got := resolveNoticeAudience("propose", config.NotifyConfig{
		Channels: []string{"rest"},
		Subjects: []string{"alice"},
	}, telegramOwner())
	if !reflect.DeepEqual(got.Channels, []string{"rest"}) || !reflect.DeepEqual(got.Subjects, []string{"alice"}) {
		t.Errorf("explicit audience was widened to %+v", got)
	}
}

// A queue with nobody to tell is still off, and saying so beats
// constructing a notifier that can never fire.
func TestNoOwnerMeansNoNudge(t *testing.T) {
	t.Parallel()
	channels := []config.GatewayChannelConfig{{Type: "telegram", UserScopes: map[string]string{"111": "public"}}}
	if resolveNoticeAudience("propose", config.NotifyConfig{}, channels).Enabled {
		t.Error("a node with no owner resolved to an audience")
	}
}
