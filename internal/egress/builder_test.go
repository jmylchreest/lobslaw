package egress

import (
	"sort"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

func TestBuildExtractsHostsFromProviderEndpoints(t *testing.T) {
	t.Parallel()
	in := ACLInputs{
		Providers: []config.ProviderConfig{
			{Label: "minimax-m2.7", Endpoint: "https://api.minimax.io/v1"},
			{Label: "qwen", Endpoint: "https://openrouter.ai/api/v1"},
		},
	}
	got := Build(in)
	if hosts := got.Roles["llm"]; !equalSet(hosts, []string{"api.minimax.io", "openrouter.ai"}) {
		t.Errorf("llm hosts = %v", hosts)
	}
	if hosts := got.Roles["llm/minimax-m2.7"]; !equalSet(hosts, []string{"api.minimax.io"}) {
		t.Errorf("llm/minimax-m2.7 hosts = %v", hosts)
	}
}

func TestBuildTelegramAlwaysAddsAPIHost(t *testing.T) {
	t.Parallel()
	in := ACLInputs{
		Channels: []config.GatewayChannelConfig{{Type: "telegram"}},
	}
	got := Build(in)
	if hosts := got.Roles["gateway/telegram"]; !equalSet(hosts, []string{"api.telegram.org"}) {
		t.Errorf("gateway/telegram = %v", hosts)
	}
}

func TestBuildSkillsPerManifest(t *testing.T) {
	t.Parallel()
	in := ACLInputs{
		SkillNetworks: map[string][]string{
			"gws-workspace": {"oauth2.googleapis.com", "*.googleapis.com"},
			"silent-skill":  nil,
		},
	}
	got := Build(in)
	if hosts := got.Roles["skill/gws-workspace"]; !equalSet(hosts, []string{"oauth2.googleapis.com", "*.googleapis.com"}) {
		t.Errorf("skill/gws-workspace = %v", hosts)
	}
	if _, ok := got.Roles["skill/silent-skill"]; !ok {
		t.Error("skill/silent-skill role should be registered (with nil hosts) so deny is reported with a useful message")
	}
}

func TestBuildClawhubDefaultsBinaryHosts(t *testing.T) {
	t.Parallel()
	got := Build(ACLInputs{ClawhubBaseURL: "https://clawhub.ai"})
	hosts := got.Roles["clawhub"]
	if len(hosts) == 0 {
		t.Fatal("clawhub role should have hosts")
	}
	wantContains := []string{"clawhub.ai", "github.com", "objects.githubusercontent.com"}
	for _, w := range wantContains {
		if !contains(hosts, w) {
			t.Errorf("clawhub hosts missing %q (got %v)", w, hosts)
		}
	}
}

func TestBuildFetchURLPermissiveByDefault(t *testing.T) {
	t.Parallel()
	got := Build(ACLInputs{})
	if !got.Permissive["fetch_url"] {
		t.Error("fetch_url should be permissive when no allowlist declared")
	}
	if _, set := got.Roles["fetch_url"]; set {
		t.Error("permissive fetch_url shouldn't have an explicit Roles entry")
	}
}

func TestBuildFetchURLLockedDownWhenConfigured(t *testing.T) {
	t.Parallel()
	got := Build(ACLInputs{FetchURLAllowHosts: []string{"api.example.com", "*.docs.example.com"}})
	if got.Permissive["fetch_url"] {
		t.Error("fetch_url should NOT be permissive when allowlist is set")
	}
	if hosts := got.Roles["fetch_url"]; !equalSet(hosts, []string{"api.example.com", "*.docs.example.com"}) {
		t.Errorf("fetch_url hosts = %v", hosts)
	}
}

func TestHostOfHandlesBareHostnames(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"https://api.minimax.io/v1": "api.minimax.io",
		"http://localhost:8080":     "localhost",
		"clawhub.ai":                "clawhub.ai", // bare; passes through
		"":                          "",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func equalSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
