package embedder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	// alt is a FLATTENED name tried when name 404s.
	//
	// GitHub release assets cannot contain "/" in their filename, so a
	// mirrored model has nowhere to put 1_Pooling/config.json. Without
	// this the file is simply absent and pooling falls back to the
	// family default — which happens to be right for all-MiniLM and is
	// exactly the kind of accident that holds until it does not. The
	// declaration is the checkpoint's, not ours to guess.
	alt string
}{
	{name: "config.json", required: true},
	{name: "model.safetensors", required: true},
	{name: "tokenizer.json", required: true},
	{name: "tokenizer_config.json"},
	{name: "1_Pooling/config.json", alt: "1_Pooling.config.json"},
	// Optional, and checked when present — see verifyChecksums.
	{name: "SHA256SUMS"},
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
			// A mirror may carry the flattened name instead; written
			// to the nested path either way, so what lands on disk is
			// a normal HuggingFace snapshot.
			if a := altOf(name); a != "" {
				if err2 := fetchAs(ctx, client, base, dir, a, name); err2 == nil {
					continue
				}
			}
			// An optional file that is genuinely absent upstream is
			// not a failure; a required one is.
			if required(name) {
				return "", explainFetchFailure(name, base, err)
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
	if err := verifyChecksums(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// verifyChecksums checks the model against a SHA256SUMS file, when the
// mirror provides one.
//
// OPPORTUNISTIC, and that is a deliberate compromise rather than an
// oversight. HuggingFace publishes no such file, so requiring one would
// mean refusing every upstream repository — and a model that cannot be
// fetched is not more secure, it is just unusable. This project's own
// mirror ships SHA256SUMS, so the configuration the docs recommend is
// verified, and anyone pointing at a mirror they control can add one.
//
// A file listed and WRONG is fatal, and the file is removed so the next
// boot re-downloads rather than loading bytes already known to be
// wrong. A file listed but absent is fine: SHA256SUMS covers what the
// mirror chose to publish, and optional files legitimately vary.
func verifyChecksums(dir string) error {
	raw, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return nil // no manifest; nothing to check against
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		want, name := fields[0], strings.TrimPrefix(fields[1], "*")
		// The manifest names a file; it must not name a PATH. A
		// mirror is a host on the internet, and "../../etc/x" in a
		// text file it controls would otherwise decide what gets read.
		if name != filepath.Base(name) {
			return fmt.Errorf("embedder: SHA256SUMS names a path (%q), not a filename", name)
		}
		path := filepath.Join(dir, name)
		f, err := os.Open(path)
		if err != nil {
			continue // listed but not fetched
		}
		sum := sha256.New()
		_, cerr := io.Copy(sum, f)
		_ = f.Close()
		if cerr != nil {
			return fmt.Errorf("embedder: read %s for verification: %w", name, cerr)
		}
		if got := hex.EncodeToString(sum.Sum(nil)); got != want {
			_ = os.Remove(path)
			return fmt.Errorf("embedder: %s failed its SHA256SUMS check (got %s, want %s) — "+
				"the file has been removed; the mirror served something other than what it published",
				name, got[:16], want[:16])
		}
	}
	return nil
}

func required(name string) bool {
	for _, f := range modelFiles {
		if f.name == name {
			return f.required
		}
	}
	return false
}

func altOf(name string) string {
	for _, f := range modelFiles {
		if f.name == name {
			return f.alt
		}
	}
	return ""
}

func fetchOne(ctx context.Context, client *http.Client, base, dir, name string) error {
	return fetchAs(ctx, client, base, dir, name, name)
}

// fetchAs downloads remote and stores it at local.
func fetchAs(ctx context.Context, client *http.Client, base, dir, remote, local string) error {
	name := local
	url := strings.TrimSuffix(base, "/") + "/" + remote
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
		return &statusError{name: name, code: resp.StatusCode}
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

// statusError is a non-200 response, carrying the code so a caller can
// tell "the mirror does not have this file" from "the request never
// arrived". explainFetchFailure needs that distinction; without it the
// two produce the same advice and one of them is wrong.
type statusError struct {
	name string
	code int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("embedder: fetch %s: HTTP %d", e.name, e.code)
}

// explainFetchFailure turns a bare transport or status error into the
// reason it happened.
//
// Two failures dominate here and they pull in opposite directions, so
// guessing wrong costs an operator real time:
//
//   - A 404 on model.safetensors usually means the repository ships
//     only pytorch_model.bin. That file is a PYTHON PICKLE, which is
//     arbitrary code execution on load, so this package will not read
//     one at any price. "HTTP 404" alone reads as a broken URL and
//     sends someone to check their typing rather than their repository.
//
//   - A proxy rejection means the request never reached the mirror.
//     The node's egress allowlist covers the host in download_url and
//     the CDNs it is known to redirect to; anything else is refused.
//     This one used to be reported with the safetensors story above —
//     a message that was not merely unhelpful but actively misleading,
//     because it blamed a repository the node had never contacted.
func explainFetchFailure(name, base string, err error) error {
	if host, ok := proxyRejectedHost(err); ok {
		return fmt.Errorf("%w\n"+
			"  the egress proxy refused a connection to %s while fetching %s.\n"+
			"  The request never reached the mirror, so this is an allowlist\n"+
			"  problem rather than a missing file: the \"embedding-model\" role\n"+
			"  allows the host in download_url plus the CDNs that host is known\n"+
			"  to redirect to. A mirror redirecting anywhere else needs that\n"+
			"  host allowed too, or the checkpoint placed on disk with\n"+
			"  download_url left empty",
			err, host, name)
	}

	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusNotFound && name == "model.safetensors" {
		return fmt.Errorf("%w\n"+
			"  %s has no model.safetensors. Many older repositories ship only\n"+
			"  pytorch_model.bin, which is a Python pickle — loading one executes\n"+
			"  arbitrary code, so it is refused rather than supported.\n"+
			"  Pick a repository that publishes safetensors, or convert it yourself",
			err, strings.TrimSuffix(base, "/"))
	}
	return err
}

// proxyRejectedHost reports the host smokescreen refused, if this error
// is a proxy rejection.
//
// Matched on the message because that is all that survives the
// http.Client boundary: the proxy answers the CONNECT with a refusal
// and the transport hands back a *url.Error wrapping it, with no typed
// error to assert on. The URL is the useful part — it names the
// REDIRECT TARGET, which is the host missing from the allowlist and
// never the one written in download_url.
func proxyRejectedHost(err error) (string, bool) {
	var ue *url.Error
	if !errors.As(err, &ue) || ue.Err == nil {
		return "", false
	}
	if !strings.Contains(ue.Err.Error(), "rejected by proxy") {
		return "", false
	}
	if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
		return u.Host, true
	}
	return ue.URL, true
}
