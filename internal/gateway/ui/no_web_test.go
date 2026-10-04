//go:build no_web

package ui

import (
	"testing"
)

func TestNoWebExcludesAssets(t *testing.T) {
	if Supported {
		t.Fatal("no_web advertises web build support")
	}
	if Built() {
		t.Fatal("no_web contains embedded assets")
	}
	if _, err := Handler(); err == nil {
		t.Fatal("no_web exposes the SPA")
	}
}
