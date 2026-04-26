package compute

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadPDFDispatchesToOpenRouter(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	pdfPath := filepath.Join(tmp, "doc.pdf")
	_ = os.WriteFile(pdfPath, []byte("%PDF-1.4\n"), 0o600)

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"a one-page document"}}]}`))
	}))
	t.Cleanup(srv.Close)

	b := NewBuiltins()
	if err := RegisterPDFBuiltin(b, PDFConfig{
		Endpoint:    srv.URL,
		Model:       "google/gemini-2.0-flash-001",
		APIKey:      "k",
		AllowedRoot: tmp,
	}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("read_pdf")
	out, code, err := fn(context.Background(), map[string]string{"path": pdfPath, "question": "what is this?"})
	if err != nil || code != 0 {
		t.Fatalf("dispatch: code=%d err=%v", code, err)
	}
	var resp map[string]any
	_ = json.Unmarshal(out, &resp)
	if resp["content"] != "a one-page document" {
		t.Errorf("content = %q", resp["content"])
	}
	if !strings.Contains(string(gotBody), `"file"`) || !strings.Contains(string(gotBody), `"file_data"`) {
		t.Errorf("expected file content part; got %s", gotBody)
	}
	if !strings.Contains(string(gotBody), `data:application/pdf;base64,`) {
		t.Errorf("expected PDF data URL; got %s", gotBody)
	}
}

func TestReadPDFRefusesPathOutsideRoot(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("PDF endpoint should not be hit when path scope rejected")
	}))
	t.Cleanup(srv.Close)

	b := NewBuiltins()
	if err := RegisterPDFBuiltin(b, PDFConfig{
		Endpoint:    srv.URL,
		Model:       "any",
		APIKey:      "k",
		AllowedRoot: "/workspace/incoming",
	}); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	out := filepath.Join(tmp, "evil.pdf")
	_ = os.WriteFile(out, []byte("%PDF"), 0o600)
	fn, _ := b.Get("read_pdf")
	_, code, err := fn(context.Background(), map[string]string{"path": out})
	if err == nil {
		t.Error("expected error")
	}
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}
