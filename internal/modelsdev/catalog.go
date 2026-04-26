// Package modelsdev consumes the community-maintained models.dev
// catalog (https://models.dev/api.json) — a unified per-model
// capability + pricing registry covering ~115 providers / ~hundreds
// of models. Used by the capability auto-discovery path so operators
// don't have to hand-maintain `capabilities = [...]` tags on every
// [[compute.providers]] entry.
//
// Trust model: the catalog is an EXTERNAL data source. Discovered
// capabilities are advisory — operator-declared capabilities ALWAYS
// win on conflict. The merge is union-with-declared-precedence,
// never replace. This protects against catalog bugs (we observed
// e.g. vercel listing MiniMax-M2.7 as multimodal when every other
// provider correctly lists it as text-only).
package modelsdev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultURL is the public catalog endpoint. Override via Fetcher
// for tests / private mirrors.
const DefaultURL = "https://models.dev/api.json"

// DefaultCacheMaxAge is how long a disk-cached catalog is treated as
// fresh. 24h matches the catalog's effective update cadence and
// avoids hammering models.dev on every node boot.
const DefaultCacheMaxAge = 24 * time.Hour

// Catalog is the parsed top-level shape: provider_id -> Provider.
type Catalog map[string]Provider

// Provider mirrors the per-provider fields in models.dev. Most are
// metadata; Models is the meat.
type Provider struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Env    []string         `json:"env,omitempty"`
	NPM    string           `json:"npm,omitempty"`
	API    string           `json:"api,omitempty"`
	Doc    string           `json:"doc,omitempty"`
	Models map[string]Model `json:"models"`
}

// Model is the per-model record. Field tags follow models.dev's
// JSON shape exactly so unmarshaling is one-shot.
type Model struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Family       string     `json:"family,omitempty"`
	Attachment   bool       `json:"attachment"`
	Reasoning    bool       `json:"reasoning"`
	ToolCall     bool       `json:"tool_call"`
	Temperature  bool       `json:"temperature,omitempty"`
	Knowledge    string     `json:"knowledge,omitempty"`
	ReleaseDate  string     `json:"release_date,omitempty"`
	LastUpdated  string     `json:"last_updated,omitempty"`
	OpenWeights  bool       `json:"open_weights,omitempty"`
	Modalities   Modalities `json:"modalities"`
	Cost         Cost       `json:"cost,omitempty"`
	Limit        Limit      `json:"limit,omitempty"`
}

// Modalities lists the input/output content types the model accepts
// and produces. Tokens we map to lobslaw capabilities:
//
//	input "text"  → "chat"
//	input "image" → "vision"
//	input "audio" → "audio-multimodal"
//	input "pdf"   → "pdf"
type Modalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

// Cost is per-million-tokens USD across the price classes.
type Cost struct {
	Input      float64 `json:"input,omitempty"`
	Output     float64 `json:"output,omitempty"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
	Reasoning  float64 `json:"reasoning,omitempty"`
}

// Limit is context window + max-output (and occasionally a separate
// input cap when the provider documents one).
type Limit struct {
	Context int `json:"context,omitempty"`
	Input   int `json:"input,omitempty"`
	Output  int `json:"output,omitempty"`
}

// Fetcher loads a Catalog from URL with disk caching. Concurrency
// safe: per-call, no shared state across calls.
type Fetcher struct {
	URL      string
	CacheDir string
	MaxAge   time.Duration
	HTTP     *http.Client
}

// NewFetcher constructs a Fetcher with sensible defaults: the
// public URL, no cache (caller passes CacheDir to enable), 24h
// freshness, and a 30s HTTP timeout.
func NewFetcher() *Fetcher {
	return &Fetcher{
		URL:    DefaultURL,
		MaxAge: DefaultCacheMaxAge,
		HTTP:   &http.Client{Timeout: 30 * time.Second},
	}
}

// Fetch returns the parsed Catalog. Lookup order:
//
//  1. CacheDir/modelsdev.json if present and younger than MaxAge.
//  2. HTTP GET to URL; on success write to cache (if CacheDir set).
//  3. Stale cache fallback (if HTTP fails AND a cache file exists,
//     return it with a logged-info-via-error wrapper so the caller
//     can decide whether to surface).
func (f *Fetcher) Fetch(ctx context.Context) (Catalog, error) {
	cachePath := f.cachePath()
	if cachePath != "" {
		if cat, ok := f.tryFreshCache(cachePath); ok {
			return cat, nil
		}
	}
	cat, fetchErr := f.fetchHTTP(ctx)
	if fetchErr == nil {
		if cachePath != "" {
			_ = f.writeCache(cachePath, cat)
		}
		return cat, nil
	}
	if cachePath != "" {
		if cat, ok := f.tryStaleCache(cachePath); ok {
			return cat, fmt.Errorf("models.dev fetch failed (using stale cache): %w", fetchErr)
		}
	}
	return nil, fetchErr
}

func (f *Fetcher) cachePath() string {
	if f.CacheDir == "" {
		return ""
	}
	return filepath.Join(f.CacheDir, "modelsdev.json")
}

func (f *Fetcher) tryFreshCache(path string) (Catalog, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	maxAge := f.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultCacheMaxAge
	}
	if time.Since(info.ModTime()) > maxAge {
		return nil, false
	}
	cat, err := readCacheFile(path)
	if err != nil {
		return nil, false
	}
	return cat, true
}

func (f *Fetcher) tryStaleCache(path string) (Catalog, bool) {
	cat, err := readCacheFile(path)
	if err != nil {
		return nil, false
	}
	return cat, true
}

func readCacheFile(path string) (Catalog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cat Catalog
	if err := json.Unmarshal(raw, &cat); err != nil {
		return nil, err
	}
	return cat, nil
}

func (f *Fetcher) writeCache(path string, cat Catalog) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(cat)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (f *Fetcher) fetchHTTP(ctx context.Context) (Catalog, error) {
	url := f.URL
	if url == "" {
		url = DefaultURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	client := f.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models.dev fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev fetch: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var cat Catalog
	if err := json.Unmarshal(raw, &cat); err != nil {
		return nil, fmt.Errorf("models.dev parse: %w", err)
	}
	return cat, nil
}

// Lookup finds a model by name. Search strategy:
//
//  1. If providerHint is non-empty, try that provider's models first.
//  2. Else (or if the hinted provider doesn't list the model), scan
//     all providers and return the FIRST exact match.
//
// providerHint is matched case-insensitively against catalog keys
// AND substring against the provider's `api` URL (so a hint like
// "openrouter.ai" finds the openrouter entry).
//
// modelName matching is exact by default; when not found, retries
// with the suffix portion after the last "/" (so callers passing
// "anthropic/claude-opus-4" can still match catalog entries keyed
// just as "claude-opus-4").
func (c Catalog) Lookup(providerHint, modelName string) (Model, bool) {
	if c == nil || modelName == "" {
		return Model{}, false
	}

	if providerHint != "" {
		if m, ok := c.lookupInProvider(providerHint, modelName); ok {
			return m, true
		}
	}
	for _, prov := range c {
		if m, ok := prov.Models[modelName]; ok {
			return m, true
		}
	}
	if idx := strings.LastIndex(modelName, "/"); idx >= 0 {
		suffix := modelName[idx+1:]
		for _, prov := range c {
			if m, ok := prov.Models[suffix]; ok {
				return m, true
			}
		}
	}
	return Model{}, false
}

func (c Catalog) lookupInProvider(hint, modelName string) (Model, bool) {
	hintLower := strings.ToLower(hint)
	for id, prov := range c {
		matchesID := strings.ToLower(id) == hintLower
		matchesAPI := prov.API != "" && strings.Contains(strings.ToLower(prov.API), hintLower)
		if !matchesID && !matchesAPI {
			continue
		}
		if m, ok := prov.Models[modelName]; ok {
			return m, true
		}
		if idx := strings.LastIndex(modelName, "/"); idx >= 0 {
			if m, ok := prov.Models[modelName[idx+1:]]; ok {
				return m, true
			}
		}
	}
	return Model{}, false
}

// LookupAll returns every catalog entry matching modelName. Useful
// for taking the INTERSECTION of capabilities when providers
// disagree (e.g. one source claims multimodal that others don't).
// Returned slice ordering is unspecified.
func (c Catalog) LookupAll(modelName string) []Model {
	if c == nil || modelName == "" {
		return nil
	}
	var out []Model
	for _, prov := range c {
		if m, ok := prov.Models[modelName]; ok {
			out = append(out, m)
		}
	}
	if idx := strings.LastIndex(modelName, "/"); idx >= 0 {
		suffix := modelName[idx+1:]
		for _, prov := range c {
			if m, ok := prov.Models[suffix]; ok {
				out = append(out, m)
			}
		}
	}
	return out
}

// ErrNotFound is returned when a model isn't in the catalog.
var ErrNotFound = errors.New("modelsdev: model not found")
