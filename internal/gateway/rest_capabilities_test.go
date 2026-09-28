package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestCapabilitiesRequiresAuth(t *testing.T) {
	t.Parallel()
	srv := startWebREST(t, &captureRunner{}, nil)
	resp := doJSON(t, http.MethodGet, webBaseURL(srv)+"/v1/capabilities", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /v1/capabilities = %d, want 401", resp.StatusCode)
	}
}

func TestCapabilitiesShape(t *testing.T) {
	t.Parallel()
	srv := startWebREST(t, &captureRunner{}, nil)
	token := mintJWTWith(t, "alice@idp", nil)
	resp := doJSON(t, http.MethodGet, webBaseURL(srv)+"/v1/capabilities", "", http.Header{
		"Authorization": []string{"Bearer " + token},
	})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body=%s", resp.StatusCode, body)
	}
	var body capabilitiesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ComputeTeams.Enabled {
		t.Error("compute-teams.enabled must be false in this story")
	}
	if body.UIWeb.Enabled {
		t.Error("ui-web.enabled must be false in this story")
	}
	if !body.Compute.Enabled {
		t.Error("compute.enabled should be true when a runner is wired")
	}
	if !body.Compute.Authorised || !body.Compute.Configured || !body.Compute.Available {
		t.Errorf("compute flags = %+v", body.Compute)
	}
	if body.ComputeTeams.Available || body.UIWeb.Available {
		t.Error("disabled capabilities must not report available=true")
	}
}

type downRunner struct {
	captureRunner
}

func (d *downRunner) Available(context.Context) bool { return false }

func TestCapabilitiesComputeUnavailableWhenBackendDown(t *testing.T) {
	t.Parallel()
	srv := startWebREST(t, &downRunner{}, nil)
	token := mintJWTWith(t, "alice@idp", nil)
	resp := doJSON(t, http.MethodGet, webBaseURL(srv)+"/v1/capabilities", "", http.Header{
		"Authorization": []string{"Bearer " + token},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body capabilitiesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Compute.Enabled || !body.Compute.Configured {
		t.Errorf("unreachable backend must stay enabled/configured, got %+v", body.Compute)
	}
	if body.Compute.Available {
		t.Error("unreachable backend must report compute.available=false, not treat records as gone")
	}
}

func TestForgedBodyUserIDIsIgnored(t *testing.T) {
	t.Parallel()
	runner := &captureRunner{}
	srv := startWebREST(t, runner, nil)
	token := mintJWTWith(t, "alice@idp", nil)
	resp := doJSON(t, http.MethodPost, webBaseURL(srv)+"/v1/messages",
		`{"message":"hi","user_id":"victim"}`, http.Header{
			"Authorization": []string{"Bearer " + token},
		})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body=%s", resp.StatusCode, body)
	}
	got := runner.lastRequest()
	if got.Claims == nil {
		t.Fatal("turn had no claims")
	}
	if got.Claims.UserID == "victim" {
		t.Fatal("request-body user_id was trusted")
	}
	if got.Claims.UserID != "alice" {
		t.Errorf("UserID = %q, want canonical alice", got.Claims.UserID)
	}
}
