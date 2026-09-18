package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/pkg/auth"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

var errCSRF = errors.New("csrf: origin mismatch")

type requestAuth struct {
	Claims     *types.Claims
	LoginID    string
	FromCookie bool
}

type loginSession struct {
	ID        string
	UserID    string
	Roles     []string
	Scope     string
	ExpiresAt time.Time
}

type loginStore struct {
	mu       sync.Mutex
	sessions map[string]*loginSession
	streams  map[string][]context.CancelFunc
	codes    map[string]loginCode
}

type loginCode struct {
	UserID    string
	Roles     []string
	Scope     string
	ExpiresAt time.Time
}

func newLoginStore() *loginStore {
	return &loginStore{
		sessions: make(map[string]*loginSession),
		streams:  make(map[string][]context.CancelFunc),
		codes:    make(map[string]loginCode),
	}
}

func (s *loginStore) put(sess *loginSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID] = sess
}

func (s *loginStore) get(id string) *loginSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	if sess == nil {
		return nil
	}
	if !sess.ExpiresAt.IsZero() && time.Now().After(sess.ExpiresAt) {
		delete(s.sessions, id)
		s.cancelLocked(id)
		return nil
	}
	return sess
}

func (s *loginStore) revoke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	s.cancelLocked(id)
}

func (s *loginStore) track(id string, cancel context.CancelFunc) {
	if id == "" || cancel == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streams[id] = append(s.streams[id], cancel)
}

func (s *loginStore) untrack(id string, cancel context.CancelFunc) {
	if id == "" || cancel == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.streams[id]
	out := cur[:0]
	want := fmt.Sprintf("%p", cancel)
	for _, c := range cur {
		if fmt.Sprintf("%p", c) != want {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		delete(s.streams, id)
		return
	}
	s.streams[id] = out
}

func (s *loginStore) cancelLocked(id string) {
	for _, c := range s.streams[id] {
		c()
	}
	delete(s.streams, id)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleSessionLogin(w, r)
	case http.MethodGet:
		s.handleSessionGet(w, r)
	case http.MethodDelete:
		s.handleSessionRevoke(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSessionLogin(w http.ResponseWriter, r *http.Request) {
	token := auth.ExtractBearer(r.Header.Get("Authorization"))
	if token != "" {
		s.loginWithJWT(w, r, token)
		return
	}
	var body struct {
		Code     string `json:"code"`
		Loopback bool   `json:"loopback"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if code := digitsOnly(body.Code); code != "" {
		s.loginWithCode(w, r, code)
		return
	}
	if body.Loopback {
		s.loginFromLoopback(w, r)
		return
	}
	s.jsonErr(w, http.StatusUnauthorized, "enter the code from `lobslaw login`, or a Bearer JWT")
}

func (s *Server) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	authn, err := s.authenticateRequest(r)
	if err != nil {
		s.jsonErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"user_id": authn.Claims.UserID})
}

func (s *Server) handleSessionRevoke(w http.ResponseWriter, r *http.Request) {
	authn, err := s.authenticateRequest(r)
	if err != nil {
		s.jsonErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err := s.checkCookieCSRF(r, authn); err != nil {
		s.jsonErr(w, http.StatusForbidden, err.Error())
		return
	}
	if authn.LoginID != "" {
		s.logins.revoke(authn.LoginID)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     LoginCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   loginCookieSecure(r, s.cfg),
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
}

func (s *Server) loginCookie(r *http.Request, sess *loginSession) *http.Cookie {
	return &http.Cookie{
		Name:     LoginCookieName,
		Value:    sess.ID,
		Path:     "/",
		Expires:  sess.ExpiresAt,
		MaxAge:   int(time.Until(sess.ExpiresAt).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   loginCookieSecure(r, s.cfg),
	}
}

func loginCookieSecure(r *http.Request, cfg RESTConfig) bool {
	if cfg.TLSCert != "" || r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) enrolledUser(ctx context.Context, jwtSub string) (config.UserConfig, bool) {
	jwtSub = strings.TrimSpace(jwtSub)
	if jwtSub == "" || len(s.cfg.Users) == 0 {
		return config.UserConfig{}, false
	}
	principal := s.resolveRESTPrincipal(ctx, jwtSub)
	for _, u := range s.cfg.Users {
		if strings.EqualFold(u.ID, principal.ID()) || strings.EqualFold(u.ID, jwtSub) {
			return u, true
		}
		for _, ch := range u.Channels {
			if strings.EqualFold(ch.Type, ChannelREST) && strings.EqualFold(ch.Address, jwtSub) {
				return u, true
			}
		}
	}
	return config.UserConfig{}, false
}

func (s *Server) resolveRESTPrincipal(ctx context.Context, jwtSub string) identity.Principal {
	if s.cfg.Identity == nil {
		return identity.User(jwtSub)
	}
	p, err := s.cfg.Identity.ResolveChannel(ctx, ChannelREST, jwtSub, jwtSub)
	if err != nil {
		s.log.Warn("rest: identity lookup failed; using the jwt subject",
			"sub", jwtSub, "err", err)
		return s.cfg.Identity.Resolve(jwtSub)
	}
	if p.IsZero() {
		return identity.User(jwtSub)
	}
	return p
}

func (s *Server) authenticateRequest(r *http.Request) (requestAuth, error) {
	if c, err := r.Cookie(LoginCookieName); err == nil && c.Value != "" {
		if sess := s.logins.get(c.Value); sess != nil {
			authn := requestAuth{
				Claims: &types.Claims{
					UserID: sess.UserID,
					Roles:  append([]string(nil), sess.Roles...),
					Scope:  sess.Scope,
				},
				LoginID:    sess.ID,
				FromCookie: true,
			}
			if authn.Claims.Scope == "" {
				authn.Claims.Scope = s.cfg.DefaultScope
			}
			return authn, nil
		}
		if s.cfg.RequireAuth && auth.ExtractBearer(r.Header.Get("Authorization")) == "" {
			return requestAuth{}, fmt.Errorf("invalid login session")
		}
	}

	claims, err := s.authenticate(r)
	if err != nil {
		return requestAuth{}, err
	}
	s.applyRESTIdentity(r.Context(), claims)
	return requestAuth{Claims: claims}, nil
}

func (s *Server) applyRESTIdentity(ctx context.Context, claims *types.Claims) {
	if claims == nil || claims.UserID == "" || claims.UserID == "anon" {
		return
	}
	if user, ok := s.enrolledUser(ctx, claims.UserID); ok {
		claims.UserID = user.ID
		return
	}
	p := s.resolveRESTPrincipal(ctx, claims.UserID)
	if !p.IsZero() {
		claims.UserID = p.ID()
	}
}

func (s *Server) checkCookieCSRF(r *http.Request, authn requestAuth) error {
	if !authn.FromCookie {
		return nil
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return errCSRF
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return errCSRF
	}
	if !strings.EqualFold(u.Host, r.Host) {
		return errCSRF
	}
	return nil
}

func (s *Server) bindStream(ctx context.Context, loginID string) (context.Context, context.CancelFunc) {
	if loginID == "" {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	s.logins.track(loginID, cancel)
	return ctx, func() {
		s.logins.untrack(loginID, cancel)
		cancel()
	}
}
