package config

import "testing"

func TestIngestIsOnlyAcceptedOnWebhooks(t *testing.T) {
	off := false
	for typ, ok := range map[string]bool{"webhook": true, "telegram": false, "rest": false} {
		cfg := Config{Gateway: GatewayConfig{Channels: []GatewayChannelConfig{{Type: typ, Ingest: &off}}}}
		if err := cfg.Validate(); (err == nil) != ok {
			t.Errorf("%s: ingest accepted=%v, want %v (%v)", typ, err == nil, ok, err)
		}
	}
}
