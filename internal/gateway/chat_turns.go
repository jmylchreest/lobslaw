package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/atomicfile"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
)

const (
	chatTurnTTL            = 24 * time.Hour
	chatTurnTimeout        = 15 * time.Minute
	chatTurnMaxRecords     = 128
	chatTurnMaxActive      = 16
	chatTurnMaxOwnerActive = 4
	chatTurnMaxBytes       = 512 << 10
)

type chatTurnRequest struct {
	ID        string `json:"id"`
	Bot       string `json:"bot,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Message   string `json:"message"`
}

type chatTurn struct {
	chatTurnRequest
	Owner     string          `json:"-"`
	State     string          `json:"state"`
	Event     string          `json:"event,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	cancel    context.CancelFunc
}

type savedChatTurn struct {
	Version int
	Owner   string
	Turn    *chatTurn
}

// The gateway keeps a bounded, encrypted reconnect journal beside its login
// store. It is node-local, not a work queue: interrupted execution is never replayed.
type chatTurns struct {
	mu      sync.Mutex
	entries map[string]*chatTurn
	dir     string
	cipher  *crypto.Cipher
	ctx     context.Context
	closed  bool
	wg      sync.WaitGroup
	log     *slog.Logger
}

func newChatTurns() *chatTurns {
	return &chatTurns{entries: make(map[string]*chatTurn), ctx: context.Background(), log: slog.Default()}
}
func chatTurnKey(owner, id string) string {
	sum := sha256.Sum256([]byte(owner + "\x00" + id))
	return hex.EncodeToString(sum[:])
}
func activeChatTurn(state string) bool { return state == "running" || state == "waiting" }

func (t *chatTurns) open(ctx context.Context, loginPath string) error {
	t.ctx = ctx
	if loginPath == "" {
		return nil
	}
	t.dir = filepath.Join(filepath.Dir(loginPath), "chat-turns")
	if err := os.MkdirAll(t.dir, 0o700); err != nil {
		return err
	}
	keyPath := filepath.Join(t.dir, "journal.key")
	raw, err := os.ReadFile(keyPath)
	if os.IsNotExist(err) {
		key, keyErr := crypto.GenerateKey()
		if keyErr != nil {
			return keyErr
		}
		raw = key[:]
		if err = atomicfile.WritePrivate(keyPath, raw); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if len(raw) != crypto.KeySize {
		return errors.New("invalid chat journal key")
	}
	t.cipher, err = crypto.NewCipher(crypto.Key(raw))
	if err != nil {
		return err
	}
	files, err := os.ReadDir(t.dir)
	if err != nil {
		return err
	}
	for _, file := range files {
		if filepath.Ext(file.Name()) != ".turn" {
			continue
		}
		turn, err := t.readRecord(file)
		if err != nil {
			t.quarantine(file.Name(), err)
			continue
		}
		if time.Since(turn.CreatedAt) >= chatTurnTTL {
			if err := os.Remove(filepath.Join(t.dir, file.Name())); err != nil {
				t.log.Warn("chat journal expired record could not be removed", "record", file.Name(), "err", err)
			}
			continue
		}
		if len(t.entries) >= chatTurnMaxRecords {
			t.log.Warn("chat journal capacity reached; skipping record", "record", file.Name())
			continue
		}
		if activeChatTurn(turn.State) {
			turn.State, turn.Event = "interrupted", "error"
			turn.Data = chatErrorData("The gateway restarted before this reply finished. Review the conversation before sending again; actions may already have run.")
			turn.UpdatedAt = time.Now().UTC()
			if err := t.save(turn); err != nil {
				t.log.Warn("chat journal interruption could not be retained", "record", file.Name(), "err", err)
			}
		}
		t.entries[chatTurnKey(turn.Owner, turn.ID)] = turn
	}
	return nil
}

func (t *chatTurns) readRecord(file os.DirEntry) (*chatTurn, error) {
	info, err := file.Info()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > chatTurnMaxBytes+128 {
		return nil, errors.New("chat journal record is not a bounded regular file")
	}
	sealed, err := os.ReadFile(filepath.Join(t.dir, file.Name()))
	if err != nil {
		return nil, err
	}
	plain, err := t.cipher.OpenTo(nil, sealed)
	if err != nil {
		return nil, fmt.Errorf("decrypt chat journal: %w", err)
	}
	var record savedChatTurn
	if err := json.Unmarshal(plain, &record); err != nil {
		return nil, errors.New("invalid chat journal JSON")
	}
	if record.Version != 1 || record.Turn == nil || record.Owner == "" || file.Name() != chatTurnKey(record.Owner, record.Turn.ID)+".turn" {
		return nil, errors.New("invalid chat journal record")
	}
	record.Turn.Owner = record.Owner
	return record.Turn, nil
}

// A reconnect cache is not a startup prerequisite. Preserve unreadable bytes
// for inspection, using an exclusively reserved name so earlier quarantines
// cannot be overwritten. A filesystem failure to quarantine still skips it.
func (t *chatTurns) quarantine(name string, cause error) {
	t.log.Warn("chat journal record unreadable; skipping", "record", name, "err", cause)
	file, err := os.CreateTemp(t.dir, name+".unreadable-*")
	if err != nil {
		t.log.Warn("chat journal record could not be quarantined", "record", name, "err", err)
		return
	}
	target := file.Name()
	err = file.Close()
	if err == nil {
		err = os.Rename(filepath.Join(t.dir, name), target)
	}
	if err != nil {
		_ = os.Remove(target)
		t.log.Warn("chat journal record could not be quarantined", "record", name, "err", err)
	}
}

func chatErrorData(message string) json.RawMessage {
	data, _ := json.Marshal(map[string]string{"message": message})
	return data
}

func (t *chatTurns) save(turn *chatTurn) error {
	if t.dir == "" {
		return nil
	}
	raw, err := json.Marshal(savedChatTurn{Version: 1, Owner: turn.Owner, Turn: turn})
	if err != nil {
		return err
	}
	if len(raw) > chatTurnMaxBytes {
		return errors.New("chat turn exceeds retention limit")
	}
	sealed, err := t.cipher.Seal(raw)
	if err != nil {
		return err
	}
	return atomicfile.WritePrivate(filepath.Join(t.dir, chatTurnKey(turn.Owner, turn.ID)+".turn"), sealed)
}

func copyChatTurn(turn *chatTurn) *chatTurn {
	copy := *turn
	copy.Data = append(json.RawMessage(nil), turn.Data...)
	copy.cancel = nil
	return &copy
}

func (t *chatTurns) get(owner, id string) *chatTurn {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn := t.entries[chatTurnKey(owner, id)]
	if turn == nil || time.Since(turn.CreatedAt) >= chatTurnTTL {
		return nil
	}
	return copyChatTurn(turn)
}

func (t *chatTurns) latest(owner, bot, session string) *chatTurn {
	t.mu.Lock()
	defer t.mu.Unlock()
	var latest *chatTurn
	for _, turn := range t.entries {
		if turn.Owner != owner || turn.Bot != bot || turn.SessionID != session || time.Since(turn.CreatedAt) >= chatTurnTTL {
			continue
		}
		if latest == nil || turn.CreatedAt.After(latest.CreatedAt) {
			latest = turn
		}
	}
	if latest == nil {
		return nil
	}
	return copyChatTurn(latest)
}

// Create is idempotent before execution starts, including after a lost HTTP response.
func (t *chatTurns) create(owner string, request chatTurnRequest, cancel context.CancelFunc) (*chatTurn, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := chatTurnKey(owner, request.ID)
	if old := t.entries[key]; old != nil {
		if old.chatTurnRequest != request {
			return nil, false, errors.New("request id already used for a different message")
		}
		return copyChatTurn(old), false, nil
	}
	active, own := 0, 0
	for key, turn := range t.entries {
		if activeChatTurn(turn.State) {
			active++
			if turn.Owner == owner {
				own++
				if turn.Bot == request.Bot && turn.SessionID == request.SessionID {
					return nil, false, errors.New("a reply is already running for this conversation")
				}
			}
		} else if time.Since(turn.CreatedAt) >= chatTurnTTL {
			if t.dir != "" {
				if err := os.Remove(filepath.Join(t.dir, key+".turn")); err != nil {
					return nil, false, err
				}
			}
			delete(t.entries, key)
		}
	}
	if t.closed || active >= chatTurnMaxActive || own >= chatTurnMaxOwnerActive || len(t.entries) >= chatTurnMaxRecords {
		return nil, false, errors.New("chat turn capacity reached; retry later")
	}
	now := time.Now().UTC()
	turn := &chatTurn{chatTurnRequest: request, Owner: owner, State: "running", CreatedAt: now, UpdatedAt: now, cancel: cancel}
	if err := t.save(turn); err != nil {
		return nil, false, err
	}
	t.entries[key] = turn
	t.wg.Add(1)
	return copyChatTurn(turn), true, nil
}

func (t *chatTurns) event(owner, id, event string, data json.RawMessage) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn := t.entries[chatTurnKey(owner, id)]
	if turn == nil || !activeChatTurn(turn.State) {
		return nil
	}
	next := *turn
	next.Event, next.Data, next.UpdatedAt = event, data, time.Now().UTC()
	switch event {
	case "needs_confirmation":
		next.State = "waiting"
	case "reply", "final", "accepted":
		next.State = "completed"
	case "error":
		next.State = "failed"
	case "working", "typing":
		// Preserve the pending confirmation across heartbeat ticks.
		if turn.State == "waiting" {
			return nil
		}
	}
	if len(data)+len(next.Message) > chatTurnMaxBytes-4096 {
		return errors.New("reply exceeds retention limit; inspect recorded conversation")
	}
	if err := t.save(&next); err != nil {
		// The worker cannot make this result durable. Release the in-memory
		// conversation rather than leaving it running until Stop or restart.
		turn.State, turn.Event = "interrupted", "error"
		turn.Data = chatErrorData("Cannot retain the reply. Inspect the recorded conversation before sending again.")
		turn.UpdatedAt = next.UpdatedAt
		return err
	}
	*turn = next
	return nil
}

func (t *chatTurns) stop(owner, id string) *chatTurn {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn := t.entries[chatTurnKey(owner, id)]
	if turn == nil {
		return nil
	}
	if activeChatTurn(turn.State) {
		turn.State, turn.Event, turn.Data = "cancelled", "error", chatErrorData("Response stopped. Any actions already performed are not undone.")
		turn.UpdatedAt = time.Now().UTC()
		if err := t.save(turn); err != nil {
			turn.State = "interrupted"
		}
		if turn.cancel != nil {
			turn.cancel()
		}
	}
	return copyChatTurn(turn)
}

func (t *chatTurns) close() {
	t.mu.Lock()
	t.closed = true
	for _, turn := range t.entries {
		if activeChatTurn(turn.State) && turn.cancel != nil {
			turn.cancel()
		}
	}
	t.mu.Unlock()
	done := make(chan struct{})
	go func() { t.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(restShutdownTimeout):
	}
}
