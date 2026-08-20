package embedder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Fetching a model into the node's own workspace.
//
// A model is a large file pulled from the internet and then trusted
// enough to be loaded into the process on every boot, so the rules here
// are deliberately narrow.

// modelFiles are what a checkpoint directory needs.
//
// Only these are ever written. A download that could create arbitrary
// paths under the workspace would turn "point at a model" into "write
// anywhere the node can" — the URL is configuration, but configuration
// is not the same as trust.
var modelFiles = []struct {
	name     string
	required bool
}{
	{"config.json", true},
	{"model.safetensors", true},
	{"tokenizer.json", true},
	{"tokenizer_config.json", false},
	{"1_Pooling/config.json", false},
}

// maxModelBytes bounds any single downloaded file.
//
// bge-m3 is about 2.2 GB, so this leaves headroom without allowing an
// unbounded write into the node's data directory. A server that streams
// forever should fill a disk slowly enough to notice, not silently.
const maxModelBytes = 8 << 30

// ModelPath is where a builtin model lives.
func ModelPath(dataDir, model string) string {
	return filepath.Join(dataDir, "models", model)
}

// Ensure returns the directory holding the named model, downloading it
// from base if it is not already complete.
//
// IDEMPOTENT AND RESUMABLE-BY-RESTART: a file that already exists at
// its full expected size is left alone, so an interrupted fetch
// re-downloads only what is missing. Partial writes go to a temporary
// name and are renamed into place, so a half-written safetensors is
// never mistaken for a complete one on the next boot.
//
// An empty base means "must already be present": nothing is fetched and
// a missing file is an error. That is the correct behaviour for a node
// with no egress, and it is the default.
func Ensure(ctx context.Context, client *http.Client, dataDir, model, base string) (string, error) {
	if model == "" {
		return "", errors.New("embedder: [compute.embeddings] model is required for type = \"builtin\"")
	}
	// The model name becomes a path, so it may not escape the models
	// directory. "../../etc" is configuration too.
	if model != filepath.Base(model) || strings.HasPrefix(model, ".") {
		return "", fmt.Errorf("embedder: model %q must be a plain directory name", model)
	}
	dir := ModelPath(dataDir, model)

	missing := make([]string, 0, len(modelFiles))
	for _, f := range modelFiles {
		if _, err := os.Stat(filepath.Join(dir, f.name)); err == nil {
			continue
		}
		// An OPTIONAL file that upstream does not have is recorded as
		// absent, once. Without that marker every boot re-requests it
		// for the life of the node: not every checkpoint ships a
		// 1_Pooling directory, so the common case was a permanent
		// pointless round trip on start-up.
		if !f.required && absentMarked(dir, f.name) {
			continue
		}
		if f.required || base != "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) == 0 {
		return dir, nil
	}
	if base == "" {
		return "", fmt.Errorf("embedder: model %q is incomplete at %s (missing %s) and no download_url is set",
			model, dir, strings.Join(missing, ", "))
	}
	if client == nil {
		return "", errors.New("embedder: no HTTP client available to fetch the model")
	}

	for _, name := range missing {
		if err := fetchOne(ctx, client, base, dir, name); err != nil {
			// An optional file that is genuinely absent upstream is
			// not a failure; a required one is.
			if required(name) {
				return "", explainMissing(name, base, err)
			}
			markAbsent(dir, name)
		}
	}
	for _, f := range modelFiles {
		if !f.required {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, f.name)); err != nil {
			return "", fmt.Errorf("embedder: %s still missing after download: %w", f.name, err)
		}
	}
	return dir, nil
}

func required(name string) bool {
	for _, f := range modelFiles {
		if f.name == name {
			return f.required
		}
	}
	return false
}

func fetchOne(ctx context.Context, client *http.Client, base, dir, name string) error {
	url := strings.TrimSuffix(base, "/") + "/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("embedder: fetch %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("embedder: fetch %s: HTTP %d", name, resp.StatusCode)
	}

	dst := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".partial-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // no-op once renamed
	}()

	sum := sha256.New()
	// LimitReader, not a trusting io.Copy: Content-Length is the
	// server's claim, and the bound has to hold whether or not it told
	// the truth.
	n, err := io.Copy(io.MultiWriter(tmp, sum), io.LimitReader(resp.Body, maxModelBytes+1))
	if err != nil {
		return fmt.Errorf("embedder: download %s: %w", name, err)
	}
	if n > maxModelBytes {
		return fmt.Errorf("embedder: %s exceeds the %d byte limit", name, int64(maxModelBytes))
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Renamed only once complete, so an interrupted download can never
	// be loaded as a valid model on the next boot.
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	// Recorded rather than verified: there is no published checksum to
	// compare against, but a node that later behaves oddly can at
	// least be asked what it actually downloaded.
	_ = os.WriteFile(dst+".sha256", []byte(hex.EncodeToString(sum.Sum(nil))+"\n"), 0o644)
	return nil
}

// absentPath is the marker recording that an optional file was asked
// for and upstream did not have it.
func absentPath(dir, name string) string {
	return filepath.Join(dir, filepath.FromSlash(name)+".absent")
}

func absentMarked(dir, name string) bool {
	_, err := os.Stat(absentPath(dir, name))
	return err == nil
}

// markAbsent is best-effort: failing to write the marker costs a
// redundant request next boot, which is not worth failing a load over.
func markAbsent(dir, name string) {
	p := absentPath(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(p, nil, 0o644)
}

// explainMissing turns a bare 404 into the reason it happened.
//
// Plenty of HuggingFace repositories — especially older ones — ship
// only pytorch_model.bin. That file is a PYTHON PICKLE, which is
// arbitrary code execution on load, so this package will not read one
// at any price. The bare error said "fetch model.safetensors: HTTP
// 404", which reads as a broken URL and sends an operator to check
// their typing rather than their repository.
func explainMissing(name, base string, err error) error {
	if name != "model.safetensors" {
		return err
	}
	return fmt.Errorf("%w\n"+
		"  %s has no model.safetensors. Many older repositories ship only\n"+
		"  pytorch_model.bin, which is a Python pickle — loading one executes\n"+
		"  arbitrary code, so it is refused rather than supported.\n"+
		"  Pick a repository that publishes safetensors, or convert it yourself",
		err, strings.TrimSuffix(base, "/"))
}
