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
	FromPeer   bool
}

type loginSession struct {
	ID        string `json:"-"`
	UserID    string
	Roles     []string
	Scope     string
	ExpiresAt time.Time
}

type loginStore struct {
	mu           sync.Mutex
	sessions     map[string]*loginSession
	streams      map[string]map[*loginStream]struct{}
	codes        map[string]loginCode
	codeAttempts int
	codeWindow   time.Time
	path         string
}

type loginStream struct{ cancel context.CancelFunc }

type loginCode struct {
	UserID    string
	Roles     []string
	Scope     string
	ExpiresAt time.Time
}

func newLoginStore() *loginStore {
	return &loginStore{
		sessions: make(map[string]*loginSession),
		streams:  make(map[string]map[*loginStream]struct{}),
		codes:    make(map[string]loginCode),
	}
}

func (s *loginStore) put(sess *loginSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := loginKey(sess.ID)
	previous := s.sessions[key]
	stored := *sess
	stored.ID = ""
	stored.Roles = append([]string(nil), sess.Roles...)
	s.sessions[key] = &stored
	if err := s.persistLocked(); err != nil {
		if previous == nil {
			delete(s.sessions, key)
		} else {
			s.sessions[key] = previous
		}
		return err
	}
	return nil
}

func (s *loginStore) get(id string) *loginSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[loginKey(id)]
	if sess == nil {
		return nil
	}
	if !sess.ExpiresAt.IsZero() && time.Now().After(sess.ExpiresAt) {
		delete(s.sessions, loginKey(id))
		s.cancelLocked(id)
		return nil
	}
	result := *sess
	result.ID = id
	result.Roles = append([]string(nil), sess.Roles...)
	return &result
}

func (s *loginStore) revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := loginKey(id)
	previous := s.sessions[key]
	delete(s.sessions, key)
	if err := s.persistLocked(); err != nil {
		if previous != nil {
			s.sessions[key] = previous
		}
		return err
	}
	s.cancelLocked(id)
	return nil
}

func (s *loginStore) track(id string, cancel context.CancelFunc) *loginStream {
	if id == "" || cancel == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	registration := &loginStream{cancel: cancel}
	sess := s.sessions[loginKey(id)]
	if sess == nil || (!sess.ExpiresAt.IsZero() && !time.Now().Before(sess.ExpiresAt)) {
		cancel()
		return registration
	}
	if s.streams[id] == nil {
		s.streams[id] = make(map[*loginStream]struct{})
	}
	s.streams[id][registration] = struct{}{}
	return registration
}

func (s *loginStore) untrack(id string, registration *loginStream) {
	if id == "" || registration == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.streams[id], registration)
	if len(s.streams[id]) == 0 {
		delete(s.streams, id)
	}
}

func (s *loginStore) cancelLocked(id string) {
	for registration := range s.streams[id] {
		registration.cancel()
	}
	delete(s.streams, id)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
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
		s.jsonErr(w, http.StatusUnauthorized, "use a sign-in code or Bearer JWT; loopback is not an identity")
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
		if s.push != nil {
			if err := s.push.RemoveLogin(authn.LoginID); err != nil {
				s.jsonErr(w, http.StatusInternalServerError, "could not revoke device notifications; try again")
				return
			}
		}
		if err := s.logins.revoke(authn.LoginID); err != nil {
			s.log.Error("rest: revoke browser session", "err", err)
			s.jsonErr(w, http.StatusInternalServerError, "could not save sign-out; try again")
			return
		}
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
	if claims, ok := r.Context().Value(forwardedConsoleIdentity{}).(*types.Claims); ok && claims != nil && claims.UserID != "" {
		return requestAuth{Claims: claims, FromPeer: true}, nil
	}
	if c, err := r.Cookie(LoginCookieName); err == nil && c.Value != "" {
		if sess := s.logins.get(c.Value); sess != nil {
			// Re-read enrollment after a restart: persisted sessions must not
			// retain access or roles removed from the node configuration.
			user, ok := s.enrolledUser(r.Context(), sess.UserID)
			if !ok {
				return requestAuth{}, fmt.Errorf("login user is no longer enrolled")
			}
			authn := requestAuth{
				Claims: &types.Claims{
					UserID: sess.UserID,
					Roles:  append([]string(nil), user.Roles...),
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

	claims, err := s.authenticate(r, s.cfg.RequireAuth)
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
	registration := s.logins.track(loginID, cancel)
	return ctx, func() {
		s.logins.untrack(loginID, registration)
		cancel()
	}
}
