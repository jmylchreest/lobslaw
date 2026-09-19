// Package ui serves the embedded web console.
//
// The React build is compiled into the binary, so an operator gets the
// console by running lobslaw and switching it on — no second
// deployable, no CORS, no version skew between an API and a front end
// shipped separately.
package ui

import (
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// ErrNotBuilt is returned when the binary was compiled without the web
// assets — `go build` without having run the Vite build first.
//
// A named error rather than a silent empty handler: a console that
// serves blank pages is far harder to diagnose than one that refuses
// to wire itself and says why.
var ErrNotBuilt = errors.New("ui: no web assets in this binary; run `make web` before building")

const (
	indexFile  = "index.html"
	distDir    = "dist"
	apiPrefix  = "/v1/"
	noCache    = "no-cache"
	htmlType   = "text/html; charset=utf-8"
	rootMarker = ""
	dotPath    = "."
)

// Built reports whether this binary carries the web assets.
//
// Exported for tests that boot a node and expect a console: without
// it, a tree where `make web` has not run fails them with a bare 404,
// which reads as a routing bug rather than as a missing build step.
func Built() bool {
	sub, err := fs.Sub(dist, distDir)
	if err != nil {
		return false
	}
	_, err = fs.Stat(sub, indexFile)
	return err == nil
}

// Handler serves the console with SPA fallback.
//
// Any path that is not a real file falls through to index.html,
// because the routes the GUI owns exist only in the browser's router.
// Without the fallback a reload on any page but the root is a 404.
func Handler() (http.Handler, error) {
	sub, err := fs.Sub(dist, distDir)
	if err != nil {
		return nil, err
	}
	if _, err := fs.Stat(sub, indexFile); err != nil {
		return nil, ErrNotBuilt
	}
	files := http.FileServer(http.FS(sub))
	return &spaHandler{fsys: sub, files: files}, nil
}

type spaHandler struct {
	fsys  fs.FS
	files http.Handler
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, apiPrefix) {
		http.NotFound(w, r)
		return
	}

	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == rootMarker || name == dotPath {
		h.serveIndex(w, r)
		return
	}
	if _, err := fs.Stat(h.fsys, name); err != nil {
		h.serveIndex(w, r)
		return
	}
	h.files.ServeHTTP(w, r)
}

func (h *spaHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	body, err := fs.ReadFile(h.fsys, indexFile)
	if err != nil {
		http.Error(w, "ui: index.html missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", htmlType)
	w.Header().Set("Cache-Control", noCache)
	_, _ = w.Write(body)
}
