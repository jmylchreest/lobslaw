package compute

import "testing"

func TestExtractAnchorsBasics(t *testing.T) {
	t.Parallel()
	html := `<html><body>
		<a href="https://example.com/1">First</a>
		<a href='/relative'>Rel</a>
		<a href="#nope">Fragment</a>
		<a href="javascript:alert(1)">Bad</a>
		<a href="mailto:a@b.com">Mail</a>
		<a href="https://example.com/1">Duplicate</a>
	</body></html>`
	links := extractAnchors(html, "https://example.com/page")

	if len(links) != 3 {
		t.Fatalf("want 3 links (1 + relative + duplicate deduped by different text); got %d: %+v", len(links), links)
	}
	// Relative should resolve against pageURL.
	found := false
	for _, l := range links {
		if l.URL == "https://example.com/relative" && l.Text == "Rel" {
			found = true
		}
	}
	if !found {
		t.Errorf("relative URL not resolved: %+v", links)
	}
}

func TestExtractAnchorsDedup(t *testing.T) {
	t.Parallel()
	html := `<a href="/x">A</a><a href="/x">A</a><a href="/x">B</a>`
	links := extractAnchors(html, "https://example.com/")
	// Same URL + same text = dedup; same URL + different text kept.
	if len(links) != 2 {
		t.Errorf("want 2 (dedup same-text); got %d: %+v", len(links), links)
	}
}

func TestExtractAnchorsSkipsBogusSchemes(t *testing.T) {
	t.Parallel()
	html := `<a href="#">F</a><a href="javascript:x()">J</a><a href="mailto:x">M</a>`
	links := extractAnchors(html, "https://example.com/")
	if len(links) != 0 {
		t.Errorf("fragment/javascript/mailto should all be dropped; got %+v", links)
	}
}

func TestHtmlToPlainWithLinksReturnsBoth(t *testing.T) {
	t.Parallel()
	html := `<html><body><h1>Title</h1><a href="https://example.com/x">Go</a></body></html>`
	plain, links := htmlToPlainWithLinks(html, "text/html", "https://example.com/")
	if plain == "" {
		t.Error("plain body empty")
	}
	if len(links) != 1 || links[0].URL != "https://example.com/x" {
		t.Errorf("links = %+v", links)
	}
}

func TestHtmlToPlainWithLinksPassthroughNonHTML(t *testing.T) {
	t.Parallel()
	body := `{"raw": "json"}`
	out, links := htmlToPlainWithLinks(body, "application/json", "")
	if out != body {
		t.Errorf("non-HTML body changed: %q", out)
	}
	if links != nil {
		t.Errorf("non-HTML should not return links; got %+v", links)
	}
}
