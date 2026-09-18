package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/auth"
)

func mediaServer(t *testing.T) (*Server, *compute.MockProvider) {
	t.Helper()
	v, err := auth.NewValidator(auth.Config{AllowHS256: true, HS256Secret: restTestSecret})
	if err != nil {
		t.Fatal(err)
	}
	provider := compute.NewMockProvider(compute.MockResponse{Content: "received"})
	agent, err := compute.NewAgent(compute.AgentConfig{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(RESTConfig{IncomingDir: t.TempDir(), JWTValidator: v, RequireAuth: true}, agent)
	t.Cleanup(s.uploads.close)
	return s, provider
}
func uploadRequest(t *testing.T, s *Server, user, mime string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/uploads", body)
	r.Header.Set("Content-Type", mime)
	if user != "" {
		r.Header.Set("Authorization", "Bearer "+mintJWTForUser(t, user))
	}
	w := httptest.NewRecorder()
	s.handleUpload(w, r)
	return w
}
func uploadID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.UploadID == "" {
		t.Fatal("missing id")
	}
	return out.UploadID
}
func TestRESTMediaUploadAndMessage(t *testing.T) {
	t.Parallel()
	for _, mime := range []string{"image/png", "audio/ogg", "audio/webm;codecs=opus"} {
		t.Run(mime, func(t *testing.T) {
			s, p := mediaServer(t)
			id := uploadID(t, uploadRequest(t, s, "alice", mime, strings.NewReader("media bytes")))
			body := `{"upload_ids":["` + id + `"]}`
			for _, user := range []string{"bob", "alice"} {
				r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+mintJWTForUser(t, user))
				w := httptest.NewRecorder()
				s.handleMessages(w, r)
				if user == "bob" {
					if w.Code != 404 || len(p.Calls()) != 0 {
						t.Fatalf("cross-user access: %d", w.Code)
					}
					continue
				}
				if w.Code != 200 {
					t.Fatalf("message: %d %s", w.Code, w.Body.String())
				}
			}
			calls := p.Calls()
			if len(calls) != 1 {
				t.Fatal(len(calls))
			}
			var content string
			for _, m := range calls[0].Messages {
				content += m.Content
			}
			if !strings.Contains(content, "path=") || !strings.Contains(content, id) {
				t.Fatalf("attachment missing: %s", content)
			}
			a, release, err := s.uploads.acquire("alice", []string{id}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			raw, err := os.ReadFile(a[0].LocalPath)
			if err != nil || string(raw) != "media bytes" {
				t.Fatalf("file: %q %v", raw, err)
			}
		})
	}
}

type uploadReader struct {
	remaining, read int64
	err             error
}

func (r *uploadReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, r.err
	}
	n := int64(len(p))
	if n > r.remaining {
		n = r.remaining
	}
	clear(p[:n])
	r.remaining -= n
	r.read += n
	return int(n), nil
}
func TestRESTUploadFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, user, mime string
		size             int64
		readErr          error
		status           int
	}{
		{"unauthenticated", "", "image/png", 100, io.EOF, 401},
		{"unsupported", "alice", "text/html", 100, io.EOF, 415},
		{"empty", "alice", "image/png", 0, io.EOF, 400},
		{"oversize", "alice", "image/png", restUploadMaxBytes + 1, io.EOF, 413},
		{"interrupted", "alice", "audio/ogg", 100, errors.New("broken stream"), 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := mediaServer(t)
			r := &uploadReader{remaining: tc.size, err: tc.readErr}
			w := uploadRequest(t, s, tc.user, tc.mime, r)
			if w.Code != tc.status {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if (tc.status == 401 || tc.status == 415) && r.read != 0 {
				t.Fatalf("read unauthorised body")
			}
			if s.uploads.used != 0 || len(s.uploads.entries) != 0 {
				t.Fatal("failed upload retained quota")
			}
			if s.uploads.dir != "" {
				files, err := os.ReadDir(s.uploads.dir)
				if err != nil || len(files) != 0 {
					t.Fatalf("partial files: %v %v", files, err)
				}
			}
		})
	}
}
func TestRESTUploadExpiryAndCapacity(t *testing.T) {
	t.Parallel()
	s, _ := mediaServer(t)
	id := uploadID(t, uploadRequest(t, s, "alice", "image/png", bytes.NewReader([]byte("image"))))
	a, release, err := s.uploads.acquire("alice", []string{id}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Hour)
	s.uploads.sweep(future)
	if _, err := os.Stat(a[0].LocalPath); err != nil {
		t.Fatal("active file removed", err)
	}
	if _, _, err := s.uploads.acquire("alice", []string{id}, future); err == nil {
		t.Fatal("accepted expired upload")
	}
	release()
	s.uploads.sweep(future)
	if _, err := os.Stat(a[0].LocalPath); !os.IsNotExist(err) {
		t.Fatal("expired file retained", err)
	}
	s.uploads.used = restUploadTotalBytes
	w := uploadRequest(t, s, "alice", "image/png", strings.NewReader("image"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatal(w.Code)
	}
}

func TestRESTUploadExactLimit(t *testing.T) {
	t.Parallel()
	s, _ := mediaServer(t)
	r := &uploadReader{remaining: restUploadMaxBytes, err: io.EOF}
	id := uploadID(t, uploadRequest(t, s, "alice", "audio/wav", r))
	a, release, err := s.uploads.acquire("alice", []string{id}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	info, err := os.Stat(a[0].LocalPath)
	if err != nil || info.Size() != restUploadMaxBytes {
		t.Fatalf("size: %v %v", info, err)
	}
	if r.read != restUploadMaxBytes {
		t.Fatal(r.read)
	}
}
func TestRESTUploadInvalidReferences(t *testing.T) {
	t.Parallel()
	s, p := mediaServer(t)
	id := uploadID(t, uploadRequest(t, s, "alice", "image/png", strings.NewReader("image")))
	for _, refs := range [][]string{{"/etc/passwd"}, {"../" + id}, {id, id}, {id, "missing"}} {
		body, _ := json.Marshal(map[string]any{"message": "look", "upload_ids": refs})
		r := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+mintJWTForUser(t, "alice"))
		w := httptest.NewRecorder()
		s.handleMessages(w, r)
		if w.Code != 404 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	if len(p.Calls()) != 0 {
		t.Fatal("invalid refs reached model")
	}
	if s.uploads.entries[id].active != 0 {
		t.Fatal("partial acquire leaked pin")
	}
}
func TestRESTUploadConcurrentReservations(t *testing.T) {
	t.Parallel()
	u := newRESTUploads(t.TempDir())
	defer u.close()
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for range 32 {
		wg.Go(func() {
			if _, err := u.reserve(); err == nil {
				mu.Lock()
				admitted++
				mu.Unlock()
			} else if !errors.Is(err, errUploadCapacity) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if admitted != 8 || u.used != restUploadTotalBytes {
		t.Fatalf("admitted %d used %d", admitted, u.used)
	}
	for range admitted {
		if err := u.finish("", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	if u.used != 0 || u.pending != 0 {
		t.Fatal("reservations leaked")
	}
}
func TestRESTUploadPeriodicExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := mediaServer(t)
		id := uploadID(t, uploadRequest(t, s, "alice", "image/png", strings.NewReader("image")))
		path := s.uploads.entries[id].attachment.LocalPath
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.uploads.run(ctx)
		synctest.Wait()
		time.Sleep(restUploadTTL + time.Minute)
		synctest.Wait()
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("expiry did not remove file", err)
		}
	})
}
func TestRESTUploadRouteAndShutdown(t *testing.T) {
	t.Parallel()
	s, _ := mediaServer(t)
	s.cfg.Addr = "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(stop)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(3 * time.Second)
	for s.Addr() == "" {
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("server did not start")
		}
	}
	r, err := http.NewRequest("POST", "http://"+s.Addr()+"/v1/uploads", strings.NewReader("image"))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "image/png")
	r.Header.Set("Authorization", "Bearer "+mintJWTForUser(t, "alice"))
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 201 {
		t.Fatal(resp.StatusCode)
	}
	var out uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	a, release, err := s.uploads.acquire("alice", []string{out.UploadID}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	path := a[0].LocalPath
	release()
	stop()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("shutdown retained file", err)
	}
}
