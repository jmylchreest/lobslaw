package oauth

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Valid JSON followed by whitespace must not bypass the response limit.
func TestOversizedResponses(t *testing.T) {
	const limit = 1 << 20
	cases := []struct {
		name, body string
		call       func(ProviderConfig) error
	}{
		{"device", `{"device_code":"d","user_code":"u"}`, func(p ProviderConfig) error { _, e := StartDeviceAuth(context.Background(), p, nil); return e }},
		{"poll", `{"access_token":"a"}`, func(p ProviderConfig) error { _, e := PollToken(context.Background(), p, "d"); return e }},
		{"refresh", `{"access_token":"a"}`, func(p ProviderConfig) error { _, e := RefreshToken(context.Background(), p, "r"); return e }},
		{"userinfo", `{"sub":"s"}`, func(p ProviderConfig) error {
			_, e := FetchSubject(context.Background(), p, &TokenResponse{AccessToken: "a"})
			return e
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, status := range []int{http.StatusOK, http.StatusBadGateway} {
				for _, extra := range []int{0, 1} {
					for _, compressed := range []bool{false, true} {
						t.Run(fmt.Sprintf("status%d/extra%d/gzip%t", status, extra, compressed), func(t *testing.T) {
							body := tc.body + strings.Repeat(" ", limit+extra-len(tc.body))
							srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
								if compressed {
									w.Header().Set("Content-Encoding", "gzip")
								}
								w.WriteHeader(status)
								w.(http.Flusher).Flush() // No Content-Length: the reader must count actual bytes.
								if compressed {
									z := gzip.NewWriter(w)
									_, _ = io.WriteString(z, body)
									_ = z.Close()
								} else {
									_, _ = io.WriteString(w, body)
								}
							}))
							defer srv.Close()
							p := ProviderConfig{Name: "test", ClientID: "cid", DeviceAuthEndpoint: srv.URL, TokenEndpoint: srv.URL, UserInfoEndpoint: srv.URL, SubjectClaim: "sub"}
							err := tc.call(p)
							switch {
							case extra == 1:
								if err == nil || !strings.Contains(err.Error(), "response body exceeds size limit") {
									t.Fatalf("expected size rejection, got %v", err)
								}
							case status == http.StatusOK && err != nil:
								t.Fatal(err)
							case status != http.StatusOK && (err == nil || strings.Contains(err.Error(), "size limit")):
								t.Fatalf("expected HTTP error, got %v", err)
							}
						})
					}
				}
			}
		})
	}
}
