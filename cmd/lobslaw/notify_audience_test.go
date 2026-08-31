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

// THE POINT. Neither the review queue nor a dream challenge should
// need a second, separately-populated block to be heard: a queue
// nobody is told about is auto mode with extra steps, and a
// contradiction nobody is asked about is dream talking to itself.
func TestTheAudienceIsDerivedWithoutBeingAskedTwice(t *testing.T) {
	t.Parallel()
	got := resolveNoticeAudience(config.NotifyConfig{}, telegramOwner())
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
	got := resolveNoticeAudience(config.NotifyConfig{}, telegramOwner())
	for _, s := range got.Subjects {
		if s == "111" || s == "tg-111" {
			t.Errorf("a public-scoped user is in the audience: %v", got.Subjects)
		}
	}
}

// Self-learning mode must not decide this.
//
// The audience used to resolve to nothing unless mode was "propose",
// which was right when the review queue was the only source. Dream
// challenges exist wherever memory does, so that gate meant switching
// self-learning to auto silently disabled every question about
// memories that disagree.
func TestSelfLearningModeDoesNotGateTheAudience(t *testing.T) {
	t.Parallel()
	if !resolveNoticeAudience(config.NotifyConfig{}, telegramOwner()).Enabled {
		t.Error("a node with an owner and a channel resolved to silence")
	}
}

// The deprecated block still works, so a config written before the
// move does not silently lose its audience.
func TestSelfLearningNotifyIsStillRead(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.SelfLearning.Notify = config.NotifyConfig{Subjects: []string{"user:old"}}
	if got := notifyConfigFor(cfg); len(got.Subjects) != 1 || got.Subjects[0] != "user:old" {
		t.Errorf("the old block was ignored: %+v", got)
	}
}

// The top-level block wins when both are set.
func TestTopLevelNotifyWins(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Notify: config.NotifyConfig{Subjects: []string{"user:new"}}}
	cfg.SelfLearning.Notify = config.NotifyConfig{Subjects: []string{"user:old"}}
	if got := notifyConfigFor(cfg); got.Subjects[0] != "user:new" {
		t.Errorf("the deprecated block overrode the current one: %+v", got)
	}
}

func TestTheNudgeCanBeTurnedOff(t *testing.T) {
	t.Parallel()
	if resolveNoticeAudience(config.NotifyConfig{Disabled: true}, telegramOwner()).Enabled {
		t.Error("notify.disabled did not silence the nudge")
	}
}

// An operator who named a channel or a subject meant that list.
// Quietly widening it would send a notice somewhere they had decided
// against.
func TestExplicitListsAreNotWidened(t *testing.T) {
	t.Parallel()
	got := resolveNoticeAudience(config.NotifyConfig{
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
	if resolveNoticeAudience(config.NotifyConfig{}, channels).Enabled {
		t.Error("a node with no owner resolved to an audience")
	}
}
