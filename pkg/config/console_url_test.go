package config

import "testing"

func TestConsoleURLAllowsLocalHTTPWithoutCredentials(t *testing.T) {
	for _, u := range []string{"", "http://192.168.5.137:8443", "https://lobslaw.example/"} {
		if err := ValidateConsolePublicURL(u); err != nil {
			t.Fatalf("%s: %v", u, err)
		}
	}
	for _, u := range []string{"javascript:alert(1)", "//host", "https://alice:secret@host", "https://host/?token=x", "https://host/#x", "https://host/subpath", " http://host"} {
		if ValidateConsolePublicURL(u) == nil {
			t.Fatalf("accepted invalid console URL %q", u)
		}
	}
}

func TestTelegramTaskNoticesRequireConsoleURL(t *testing.T) {
	cfg := Config{Gateway: GatewayConfig{Channels: []GatewayChannelConfig{{Type: "telegram", NotifyTasks: true}}}}
	if cfg.Validate() == nil {
		t.Fatal("task notices accepted without a console link")
	}
	cfg.UIWeb.PublicURL = "http://192.168.5.137:8443"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}
