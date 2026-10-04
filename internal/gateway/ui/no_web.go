//go:build no_web

// Package ui reports embedded web support for this build.
package ui

import (
	"errors"
	"net/http"
)

const Supported = false

var ErrNotBuilt = errors.New("ui: web support excluded by no_web build tag; use a web-enabled binary")

func Built() bool                    { return false }
func Handler() (http.Handler, error) { return nil, ErrNotBuilt }
