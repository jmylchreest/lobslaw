package memory

import (
	"context"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/turn"
)

func TestConnectorCredentialIsolation(t *testing.T) {
	t.Parallel()
	s := newTestCredentialService(t)
	a := turn.WithIdentity(context.Background(), turn.Identity{Principal: "alice"})
	b := turn.WithIdentity(context.Background(), turn.Identity{Principal: "bob"})
	c := s.ForConnector("google-calendar")
	p := &PlaintextCredential{Provider: "google", Subject: "connection-a", AccessToken: "access", RefreshToken: "refresh", Scopes: []string{"calendar"}, ExpiresAt: time.Now().Add(time.Hour)}
	if err := c.Put(a, p); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(b, "google", p.Subject); err == nil {
		t.Fatal("cross-owner read allowed")
	}
	if _, err := c.Get(context.Background(), "google", p.Subject); err == nil {
		t.Fatal("anonymous read allowed")
	}
	if _, err := s.Get(a, "google", p.Subject); err == nil {
		t.Fatal("legacy API exposed connector token")
	}
	if err := s.Grant(a, "google", p.Subject, "malicious-skill", []string{"calendar"}); err == nil {
		t.Fatal("skill grant exposed connector token")
	}
	if _, err := s.IssueForSkill(a, "google", p.Subject, "google-calendar", nil); err == nil {
		t.Fatal("skill impersonated connector")
	}
	if err := s.Delete(a, "google", p.Subject); err == nil {
		t.Fatal("legacy API deleted connector credential")
	}
	if err := s.Put(a, &PlaintextCredential{Provider: "google", Subject: p.Subject}); err == nil {
		t.Fatal("legacy API replaced connector credential")
	}
	if list, err := s.List(a); err != nil || len(list) != 0 {
		t.Fatalf("legacy list: %d %v", len(list), err)
	}
	if list, err := c.List(b); err != nil || len(list) != 0 {
		t.Fatalf("cross-owner list: %d %v", len(list), err)
	}
	if _, err := c.Issue(a, "google", p.Subject, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(b, "google", p.Subject); err == nil {
		t.Fatal("cross-owner delete allowed")
	}
	if err := c.Delete(a, "google", p.Subject); err != nil {
		t.Fatal(err)
	}
}
