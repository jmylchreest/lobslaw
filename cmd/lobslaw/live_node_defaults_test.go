package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"
	"time"
)

func TestLiveNodeTimeoutDefaultMatchesHelpAndCanBeOverridden(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		preset time.Duration
		want   time.Duration
	}{
		{"normal", 0, defaultRPCTimeout},
		{"restore", defaultRestoreTimeout, defaultRestoreTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			node := liveNode{timeout: tc.preset}
			fs := flag.NewFlagSet(tc.name, flag.ContinueOnError)
			node.bind(fs)
			var help bytes.Buffer
			fs.SetOutput(&help)
			fs.PrintDefaults()
			if node.timeout != tc.want || fs.Lookup("timeout").DefValue != tc.want.String() {
				t.Fatalf("effective/default timeout = %s/%s; want %s", node.timeout, fs.Lookup("timeout").DefValue, tc.want)
			}
			if !strings.Contains(help.String(), "(default "+tc.want.String()+")") {
				t.Fatalf("help does not advertise timeout %s: %s", tc.want, help.String())
			}
			const override = 47 * time.Second
			if err := fs.Parse([]string{"--timeout", override.String()}); err != nil {
				t.Fatal(err)
			}
			if node.timeout != override {
				t.Errorf("explicit timeout = %s, want %s", node.timeout, override)
			}
		})
	}
}
