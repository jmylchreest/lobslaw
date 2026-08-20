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

func TestBuildOAuthRolesScopedToProviderHosts(t *testing.T) {
	t.Parallel()
	in := ACLInputs{
		OAuthProviders: map[string]OAuthEndpoints{
			"google": {
				DeviceAuthEndpoint: "https://oauth2.googleapis.com/device/code",
				TokenEndpoint:      "https://oauth2.googleapis.com/token",
			},
			"github": {
				DeviceAuthEndpoint: "https://github.com/login/device/code",
				TokenEndpoint:      "https://github.com/login/oauth/access_token",
			},
		},
	}
	got := Build(in)
	if hosts := got.Roles["oauth/google"]; !equalSet(hosts, []string{"oauth2.googleapis.com"}) {
		t.Errorf("oauth/google = %v", hosts)
	}
	if hosts := got.Roles["oauth/github"]; !equalSet(hosts, []string{"github.com"}) {
		t.Errorf("oauth/github = %v", hosts)
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

// The catalog fetcher calls egress.For("modelsdev"). Without a
// matching role the proxy refused it, auto_capabilities logged
// "Request rejected by proxy", and every opted-in provider silently
// fell back to hand-declared capabilities — a feature wired end to
// end except for the line that let it out of the box.
func TestModelDiscoveryGetsAnEgressRoleWhenAsked(t *testing.T) {
	t.Parallel()
	rules := Build(ACLInputs{
		WantsModelDiscovery: true,
		ModelsDevURL:        "https://models.dev/api.json",
	})
	hosts, ok := rules.Roles["modelsdev"]
	if !ok {
		t.Fatal("no modelsdev role; the catalog fetch is refused by our own proxy")
	}
	if len(hosts) != 1 || hosts[0] != "models.dev" {
		t.Errorf("modelsdev hosts = %v, want just the catalog host", hosts)
	}
}

// A node that never fetches the catalog should not be allowed to.
func TestNoDiscoveryRoleWhenNobodyOptedIn(t *testing.T) {
	t.Parallel()
	rules := Build(ACLInputs{ModelsDevURL: "https://models.dev/api.json"})
	if _, ok := rules.Roles["modelsdev"]; ok {
		t.Error("a node with no auto_capabilities provider carries an allowance it never uses")
	}
}

// A private mirror gets the allowance instead of the public host —
// otherwise an operator pointing at their own catalog is refused by
// the proxy while the public one they are not using is permitted.
func TestAPrivateCatalogMirrorGetsTheAllowance(t *testing.T) {
	t.Parallel()
	rules := Build(ACLInputs{
		WantsModelDiscovery: true,
		ModelsDevURL:        "https://catalog.internal.example.com/api.json",
	})
	if got := rules.Roles["modelsdev"]; len(got) != 1 || got[0] != "catalog.internal.example.com" {
		t.Errorf("modelsdev hosts = %v, want the mirror", got)
	}
}

// A builtin embedding model is DOWNLOADED, and a download that the
// policy has no rule for is denied.
//
// The "embedding" role already existed and is the wrong one: it carries
// the LLM provider hosts, because it is the allowance for CALLING an
// embedding API. A model comes from a mirror. Reusing it would have
// granted the wrong hosts and denied the right one.
//
// This is the same failure the modelsdev role was added to fix — wired
// end to end except for the line that lets it out of the box — and it
// happened again here, so it gets a test.
func TestABuiltinModelDownloadGetsItsOwnRole(t *testing.T) {
	t.Parallel()
	rules := Build(ACLInputs{EmbeddingModelURL: "https://models.example.org/e5/base"})
	hosts, ok := rules.Roles["embedding-model"]
	if !ok {
		t.Fatal("no embedding-model role; the download would be denied under Enforce")
	}
	if len(hosts) != 1 || hosts[0] != "models.example.org" {
		t.Errorf("embedding-model hosts = %v, want [models.example.org]", hosts)
	}
	// It must NOT be conflated with the API-calling role.
	if _, clash := rules.Roles["embedding"]; clash && len(rules.Roles["embedding"]) > 0 {
		for _, h := range rules.Roles["embedding"] {
			if h == "models.example.org" {
				t.Error("the download host leaked into the embedding API role")
			}
		}
	}
}

// A node that ships its model on disk fetches nothing, so it must carry
// no allowance for fetching one. An unconditional rule would widen
// every air-gapped node's egress for a feature it does not use.
func TestNoDownloadURLGrantsNoModelEgress(t *testing.T) {
	t.Parallel()
	rules := Build(ACLInputs{})
	if hosts, ok := rules.Roles["embedding-model"]; ok {
		t.Errorf("embedding-model role granted with no download_url: %v", hosts)
	}
}
