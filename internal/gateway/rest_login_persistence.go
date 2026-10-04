package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jmylchreest/lobslaw/internal/atomicfile"
)

// Only token hashes reach disk. A copy of the session file is not a collection
// of bearer credentials. Codes and active stream registrations stay ephemeral.
func loginKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

type loginSnapshot struct {
	Version  int
	Sessions map[string]*loginSession
}

func (s *loginStore) load(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		s.path = path
		return nil
	}
	if err != nil {
		return err
	}
	var snapshot loginSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return err
	}
	if snapshot.Version != 1 || snapshot.Sessions == nil {
		return fmt.Errorf("invalid browser-session snapshot")
	}
	for key, sess := range snapshot.Sessions {
		if sess == nil || sess.UserID == "" || !time.Now().Before(sess.ExpiresAt) {
			delete(snapshot.Sessions, key)
		}
	}
	s.sessions, s.path = snapshot.Sessions, path
	return nil
}

// Caller holds mu. Flush a private temporary file before atomically replacing
// the snapshot, so a failed write never publishes a partially saved login.
func (s *loginStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	sessions := make(map[string]*loginSession, len(s.sessions))
	for key, sess := range s.sessions {
		if time.Now().Before(sess.ExpiresAt) {
			sessions[key] = sess
		}
	}
	raw, err := json.Marshal(loginSnapshot{Version: 1, Sessions: sessions})
	if err != nil {
		return err
	}
	return atomicfile.WritePrivate(s.path, raw)
}
