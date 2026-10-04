package gateway

import (
	"log/slog"
	"net/http"

	"github.com/jmylchreest/lobslaw/pkg/auth"
)

// statusRecorder captures the status code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush must be forwarded: the SSE handlers type-assert http.Flusher,
// and a wrapper that hides it silently downgrades streaming to a
// buffered JSON response. Same reason for Unwrap, which the standard
// http.ResponseController walks.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// withAccessLog logs one line per request at Debug. It deliberately
// records only PRESENCE of credentials, never their values — enough to
// answer "did the browser send the cookie", which is otherwise
// invisible.
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		_, hasCookieErr := r.Cookie(LoginCookieName)
		s.log.Log(r.Context(), slog.LevelDebug, "rest: request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"remote", r.RemoteAddr,
			"cookie", hasCookieErr == nil,
			"bearer", auth.ExtractBearer(r.Header.Get("Authorization")) != "",
		)
	})
}
