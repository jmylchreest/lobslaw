package gateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
)

func TestGatewayStartsWithUnreadableChatJournalRecords(t *testing.T) {
	for _, damage := range []string{"ciphertext", "json", "missing-key", "replaced-key"} {
		t.Run(damage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auth", "browser-sessions.json")
			journal := newChatTurns()
			if err := journal.open(t.Context(), path); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"good", "bad"} {
				_, created, err := journal.create("alice", chatTurnRequest{ID: id, Bot: id, Message: "journal-private-message"}, func() {})
				if err != nil || !created {
					t.Fatalf("create %s: %v", id, err)
				}
				journal.wg.Done()
				if err := journal.event("alice", id, "reply", json.RawMessage(`{"text":"journal-private-result"}`)); err != nil {
					t.Fatal(err)
				}
			}
			badPath := filepath.Join(journal.dir, chatTurnKey("alice", "bad")+".turn")
			keyPath := filepath.Join(journal.dir, "journal.key")
			keyChanged := damage == "missing-key" || damage == "replaced-key"
			switch damage {
			case "ciphertext":
				if err := os.WriteFile(badPath, []byte("corrupted ciphertext"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "json":
				sealed, err := journal.cipher.Seal([]byte("journal-private-invalid-json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(badPath, sealed, 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing-key":
				if err := os.Remove(keyPath); err != nil {
					t.Fatal(err)
				}
			case "replaced-key":
				key, err := crypto.GenerateKey()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(keyPath, key[:], 0o600); err != nil {
					t.Fatal(err)
				}
			}
			badBytes, err := os.ReadFile(badPath)
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
			server := startWebREST(t, &captureRunner{}, func(c *RESTConfig) { c.LoginSessionFile, c.Logger = path, logger })
			response := doJSON(t, http.MethodGet, webBaseURL(server)+"/healthz", "", nil)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("gateway health: %d", response.StatusCode)
			}
			if server.chatTurns.get("alice", "bad") != nil {
				t.Fatal("unreadable record was admitted")
			}
			assertHealthyJournalRecord(t, server.chatTurns, keyChanged)
			quarantines, err := filepath.Glob(badPath + ".unreadable-*")
			if err != nil || len(quarantines) != 1 {
				t.Fatalf("bad record not quarantined: %v %v", quarantines, err)
			}
			preserved, err := os.ReadFile(quarantines[0])
			if err != nil || !bytes.Equal(preserved, badBytes) {
				t.Fatal("quarantine lost the original bytes", err)
			}
			if !strings.Contains(logs.String(), "record unreadable") || strings.Contains(logs.String(), "journal-private") {
				t.Fatalf("quarantine logging missing or exposed content: %s", logs.String())
			}
			reopened := newChatTurns()
			if err := reopened.open(t.Context(), path); err != nil {
				t.Fatal("quarantined records prevented another restart", err)
			}
			assertHealthyJournalRecord(t, reopened, keyChanged)
		})
	}
}

func assertHealthyJournalRecord(t *testing.T, journal *chatTurns, keyChanged bool) {
	t.Helper()
	good := journal.get("alice", "good")
	if keyChanged {
		if good != nil {
			t.Fatal("record sealed with the old key was admitted")
		}
		return
	}
	if good == nil || good.State != "completed" || !strings.Contains(string(good.Data), "journal-private-result") {
		t.Fatal("healthy cached reply was lost")
	}
	if journal.get("bob", "good") != nil {
		t.Fatal("cached reply leaked across owners")
	}
}

func TestChatJournalFailedEventReleasesConversationWithoutReplay(t *testing.T) {
	journal := newChatTurns()
	if err := journal.open(t.Context(), filepath.Join(t.TempDir(), "sessions.json")); err != nil {
		t.Fatal(err)
	}
	request := chatTurnRequest{ID: "first", Bot: "worker", Message: "work"}
	if _, created, err := journal.create("alice", request, func() {}); err != nil || !created {
		t.Fatal(err)
	}
	journal.wg.Done()
	// Make writes fail independently of the effective uid or directory modes.
	backup := journal.dir + ".saved"
	if err := os.Rename(journal.dir, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal.dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := journal.event("alice", "first", "reply", json.RawMessage(`{"text":"done"}`)); err == nil {
		t.Fatal("expected persistence failure")
	}
	if got := journal.get("alice", "first"); got.State != "interrupted" || got.Event != "error" {
		t.Fatalf("failed write left conversation active: %+v", got)
	}
	if err := os.Remove(journal.dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, journal.dir); err != nil {
		t.Fatal(err)
	}
	if _, created, err := journal.create("alice", request, func() {}); err != nil || created {
		t.Fatal("interrupted request was replayed", err)
	}
	request.ID = "next"
	if _, created, err := journal.create("alice", request, func() {}); err != nil || !created {
		t.Fatal("conversation was not released after failed save", err)
	}
	journal.wg.Done()
}
