package clawhub

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewClientRequiresBaseURL(t *testing.T) {
	t.Parallel()
	if _, err := NewClient(""); err == nil {
		t.Error("empty base URL should fail")
	}
}

func TestGetSkillSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/skills/gws-workspace/1.2.3" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{
			"name":"gws-workspace",
			"version":"1.2.3",
			"description":"Google Workspace skill",
			"bundle_url":"https://cdn.example/gws-workspace-1.2.3.tgz",
			"bundle_sha256":"deadbeef",
			"signed_by":"alice",
			"signature":"abc"
		}`)
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := c.GetSkill(context.Background(), "gws-workspace", "1.2.3")
	if err != nil {
		t.Fatalf("GetSkill: %v", err)
	}
	if entry.Name != "gws-workspace" || entry.Version != "1.2.3" {
		t.Errorf("entry = %+v", entry)
	}
	if entry.BundleSHA256 != "deadbeef" {
		t.Errorf("sha = %q", entry.BundleSHA256)
	}
}

func TestGetSkill404IsClearError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c, _ := NewClient(srv.URL)
	_, err := c.GetSkill(context.Background(), "ghost", "1.0.0")
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should mention skill name: %v", err)
	}
}

func TestGetSkillRejectsTraversalInName(t *testing.T) {
	t.Parallel()
	c, _ := NewClient("https://example.invalid")
	cases := []string{"..", "../etc", "foo/bar", "foo\\bar"}
	for _, name := range cases {
		if _, err := c.GetSkill(context.Background(), name, "1.0.0"); err == nil {
			t.Errorf("name %q should be rejected", name)
		}
	}
}

func TestGetSkillRejectsMissingFields(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"name":"x"}`)
	}))
	t.Cleanup(srv.Close)

	c, _ := NewClient(srv.URL)
	_, err := c.GetSkill(context.Background(), "x", "1.0.0")
	if err == nil {
		t.Fatal("expected error for catalog response missing fields")
	}
}

func TestDownloadBundleStreamsBody(t *testing.T) {
	t.Parallel()
	payload := strings.Repeat("a", 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(srv.Close)

	c, _ := NewClient("https://example.invalid")
	entry := &SkillEntry{BundleURL: srv.URL}
	body, err := c.DownloadBundle(context.Background(), entry)
	if err != nil {
		t.Fatalf("DownloadBundle: %v", err)
	}
	t.Cleanup(func() { _ = body.Close() })
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != payload {
		t.Errorf("body length = %d, want %d", len(got), len(payload))
	}
}

func TestDownloadBundleRejectsNilEntry(t *testing.T) {
	t.Parallel()
	c, _ := NewClient("https://example.invalid")
	_, err := c.DownloadBundle(context.Background(), nil)
	if err == nil {
		t.Error("nil entry should fail")
	}
}

func TestDownloadBundleSurfacesHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c, _ := NewClient("https://example.invalid")
	_, err := c.DownloadBundle(context.Background(), &SkillEntry{BundleURL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("expected HTTP 500 surfaced; got %v", err)
	}
}

func TestValidateSkillIdentifierRejectsEmpty(t *testing.T) {
	t.Parallel()
	if err := validateSkillIdentifier(""); !errors.Is(err, err) || err == nil {
		t.Error("empty should fail")
	}
}
