package egress

import (
	"slices"
	"testing"
)

func TestWebPushRoleOnlyAllowsBrowserVendorsWhenEnabled(t *testing.T) {
	if _, ok := Build(ACLInputs{}).Roles["gateway/web-push"]; ok {
		t.Fatal("push enabled without web console")
	}
	hosts := Build(ACLInputs{WebPush: true}).Roles["gateway/web-push"]
	if !slices.Equal(hosts, []string{"fcm.googleapis.com", "updates.push.services.mozilla.com", "web.push.apple.com"}) {
		t.Fatalf("unexpected push egress: %v", hosts)
	}
}
