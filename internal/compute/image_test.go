package compute

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func imageServer(t *testing.T, status int, body string, record *imageWire) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if record != nil {
			_ = json.NewDecoder(r.Body).Decode(record)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newImageDriver(t *testing.T, endpoint string, preferURL bool) *OpenAIImageDriver {
	t.Helper()
	d, err := NewOpenAIImageDriver(OpenAIImageConfig{
		Endpoint:   endpoint,
		Model:      "gpt-image-1",
		Credential: NewBearerCredential("k"),
		PreferURL:  preferURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Image is the first driver that returns two different delivery modes
// from one endpoint, so it is the one that shows the resolver earning
// its keep rather than being speculative.
func TestImageReturnsEitherDeliveryMode(t *testing.T) {
	t.Parallel()

	t.Run("inline base64", func(t *testing.T) {
		t.Parallel()
		b64 := base64.StdEncoding.EncodeToString([]byte("PNGDATA"))
		var sent imageWire
		srv := imageServer(t, http.StatusOK, `{"data":[{"b64_json":"`+b64+`"}]}`, &sent)

		art, err := newImageDriver(t, srv.URL, false).Generate(
			context.Background(), ImageRequest{Prompt: "a cube"})
		if err != nil {
			t.Fatal(err)
		}
		if art.Kind != ArtifactInline || string(art.Bytes) != "PNGDATA" {
			t.Errorf("artifact = %+v, want decoded inline bytes", art)
		}
		if sent.ResponseFormat != "b64_json" {
			t.Errorf("response_format = %q, want b64_json", sent.ResponseFormat)
		}
		if sent.N != 1 {
			t.Errorf("n = %d, want 1 — more than one image would bill for pictures nobody asked for", sent.N)
		}
	})

	t.Run("hosted url", func(t *testing.T) {
		t.Parallel()
		var sent imageWire
		srv := imageServer(t, http.StatusOK, `{"data":[{"url":"https://example.invalid/x.png"}]}`, &sent)

		art, err := newImageDriver(t, srv.URL, true).Generate(
			context.Background(), ImageRequest{Prompt: "a cube"})
		if err != nil {
			t.Fatal(err)
		}
		if art.Kind != ArtifactURL || art.URL == "" {
			t.Errorf("artifact = %+v, want a URL artifact", art)
		}
		if sent.ResponseFormat != "url" {
			t.Errorf("response_format = %q, want url", sent.ResponseFormat)
		}
	})
}

func TestImageClassifiesFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   FailureClass
	}{
		{"server error", 503, `{"error":"upstream"}`, FailureTransient},
		{"quota", 429, `{"error":{"type":"insufficient_quota"}}`, FailureQuotaExhausted},
		{"content policy", 400, `{"error":"safety system rejected this prompt"}`, FailurePermanent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := imageServer(t, tc.status, tc.body, nil)
			_, err := newImageDriver(t, srv.URL, false).Generate(
				context.Background(), ImageRequest{Prompt: "x"})
			if err == nil {
				t.Fatalf("HTTP %d produced no error", tc.status)
			}
			if got := ClassifyFailure(err); got != tc.want {
				t.Errorf("HTTP %d classified %s, want %s", tc.status, got, tc.want)
			}
		})
	}
}

// A 200 with an empty data array is a provider bug rather than a
// caller one, so the backup is worth trying.
func TestImageEmptyDataIsTransient(t *testing.T) {
	t.Parallel()
	srv := imageServer(t, http.StatusOK, `{"data":[]}`, nil)
	_, err := newImageDriver(t, srv.URL, false).Generate(
		context.Background(), ImageRequest{Prompt: "x"})
	if err == nil {
		t.Fatal("an empty data array was accepted")
	}
	if got := ClassifyFailure(err); got != FailureTransient {
		t.Errorf("classified %s, want transient", got)
	}
}

func TestImageRejectsEmptyPromptPermanently(t *testing.T) {
	t.Parallel()
	srv := imageServer(t, http.StatusOK, `{"data":[{"b64_json":"eA=="}]}`, nil)
	_, err := newImageDriver(t, srv.URL, false).Generate(
		context.Background(), ImageRequest{Prompt: "  "})
	if err == nil {
		t.Fatal("an empty prompt reached the provider")
	}
	if got := ClassifyFailure(err); got != FailurePermanent {
		t.Errorf("classified %s, want permanent", got)
	}
}
