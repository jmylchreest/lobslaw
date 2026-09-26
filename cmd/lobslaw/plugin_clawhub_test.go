package main

import (
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

func TestPluginClawhubRequiresReviewedSharedInstall(t *testing.T) {
	for _, tc := range []struct {
		root string
		yes  bool
		want string
	}{{"/tmp/legacy", false, "--root"}, {"", true, "--yes"}, {"", false, "--owner"}} {
		err := pluginInstallClawhub("clawhub:demo", tc.root, tc.yes, &shareInstallOptions{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("expected %s rejection before retrieval, got %v", tc.want, err)
		}
	}
}

func TestDoctorClawhubDoesNotRequireWatchedMount(t *testing.T) {
	cfg := new(config.Config)
	cfg.Security.ClawhubBaseURL = "https://example.test"
	message, err := (doctorEnv{cfg: cfg}).checkSkillMounts()
	if err != nil || strings.Contains(message, "clawhub install will fail") {
		t.Fatalf("obsolete ClawHub mount requirement: %s %v", message, err)
	}
}
