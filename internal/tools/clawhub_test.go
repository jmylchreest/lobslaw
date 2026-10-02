package tools

import (
	"context"
	"encoding/json"
	"testing"
)

func TestClawhubBuiltinOnlyProposes(t *testing.T) {
	calls := 0
	handler := newClawhubInstallHandler(ClawhubConfig{Propose: func(_ context.Context, ref string) ([]byte, error) {
		calls++
		if ref != "clawhub:owner/demo" {
			t.Fatalf("ref %q", ref)
		}
		return []byte(`{"installation_id":"share-demo","activation_required":true}`), nil
	}})
	raw, code, err := handler(context.Background(), map[string]string{"slug": "owner/demo"})
	if err != nil || code != 0 {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result["activation_required"] != true {
		t.Fatal("proposal result lost")
	}
	for _, key := range []string{"activate", "owner", "mount", "subpath", "bootstrap_managers", "expected_plan"} {
		if _, _, err := handler(context.Background(), map[string]string{"slug": "demo", key: "true"}); err == nil {
			t.Fatalf("legacy or privilege-changing argument %q accepted", key)
		}
	}
	if calls != 1 {
		t.Fatal("rejected request reached proposal writer")
	}
}
