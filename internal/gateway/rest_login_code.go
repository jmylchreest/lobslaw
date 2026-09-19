package gateway

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

func (s *Server) handleSessionCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requestIsLoopback(r) {
		s.jsonErr(w, http.StatusForbidden, "sign-in codes can only be minted from this machine")
		return
	}
	var body struct {
		User string `json:"user"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	user, ok := s.loginUser(body.User)
	if !ok {
		s.jsonErr(w, http.StatusForbidden, "no enrolled user to issue a code for")
		return
	}
	code, err := s.logins.issueCode(user, s.cfg.DefaultScope, LoginCodeTTL)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":       code,
		"user_id":    user.ID,
		"expires_in": int(LoginCodeTTL.Seconds()),
	})
}

func (s *Server) loginWithJWT(w http.ResponseWriter, r *http.Request, token string) {
	if s.cfg.JWTValidator == nil {
		s.jsonErr(w, http.StatusUnauthorized, "auth required but no validator configured")
		return
	}
	claims, err := s.cfg.JWTValidator.Validate(token)
	if err != nil {
		s.jsonErr(w, http.StatusUnauthorized, "that token was not accepted")
		return
	}
	user, ok := s.enrolledUser(r.Context(), claims.UserID)
	if !ok {
		s.jsonErr(w, http.StatusForbidden, "that token is not for an enrolled user")
		return
	}
	s.issueLoginCookie(w, r, user, claims.Scope)
}

// MintLoginCode issues a one-time console sign-in code for the account
// a principal names. Exposed so the agent can hand the operator a code
// from a channel they are already talking to; the caller must have
// checked that the asker is an operator.
func (s *Server) MintLoginCode(ctx context.Context, principal string) (string, string, int, error) {
	if !s.consoleEnabled() {
		return "", "", 0, errors.New("the web console is not enabled on this node")
	}
	if s.cfg.JWTValidator == nil {
		return "", "", 0, errors.New("console sign-in is not configured on this node")
	}
	p := strings.TrimSpace(principal)
	// A turn run as a bot asks on behalf of its human. The code belongs
	// to the owner, not the bot — a bot has no console account.
	if strings.HasPrefix(p, identity.KindBot+":") && s.cfg.Bots != nil {
		if rec, err := s.cfg.Bots.Get(ctx, strings.TrimPrefix(p, identity.KindBot+":")); err == nil && rec != nil {
			p = strings.TrimSpace(rec.GetOwner())
		}
	}
	id := strings.TrimPrefix(p, identity.KindUser+":")
	user, ok := s.loginUser(id)
	if !ok {
		return "", "", 0, errors.New("no enrolled user matches this account")
	}
	code, err := s.logins.issueCode(user, s.cfg.DefaultScope, LoginCodeTTL)
	if err != nil {
		return "", "", 0, err
	}
	return code, user.ID, int(LoginCodeTTL.Seconds()), nil
}

func (s *Server) loginWithCode(w http.ResponseWriter, r *http.Request, code string) {
	userID, roles, scope, ok := s.logins.consumeCode(code)
	if !ok {
		s.log.Warn("rest: sign-in code rejected", "remote", r.RemoteAddr, "digits", len(code))
		s.jsonErr(w, http.StatusUnauthorized, "that code is wrong or has expired — run `lobslaw login` again")
		return
	}
	s.log.Info("rest: sign-in code accepted", "user", userID, "remote", r.RemoteAddr)
	s.issueLoginCookie(w, r, config.UserConfig{ID: userID, Roles: roles}, scope)
}

func (s *Server) loginFromLoopback(w http.ResponseWriter, r *http.Request) {
	if !requestIsLoopback(r) {
		s.log.Warn("rest: loopback sign-in refused", "remote", r.RemoteAddr)
		s.jsonErr(w, http.StatusForbidden, "this sign-in only works from the computer running the node")
		return
	}
	user, ok := s.loginUser("")
	if !ok {
		s.jsonErr(w, http.StatusForbidden, "no enrolled [[user]] to sign in as")
		return
	}
	s.issueLoginCookie(w, r, user, s.cfg.DefaultScope)
}

func (s *Server) issueLoginCookie(w http.ResponseWriter, r *http.Request, user config.UserConfig, scope string) {
	sess := &loginSession{
		ID:        loginSessionIDPrefix + ids.New(),
		UserID:    user.ID,
		Roles:     append([]string(nil), user.Roles...),
		Scope:     scope,
		ExpiresAt: time.Now().Add(DefaultLoginSessionTTL),
	}
	if sess.Scope == "" {
		sess.Scope = s.cfg.DefaultScope
	}
	s.logins.put(sess)
	http.SetCookie(w, s.loginCookie(r, sess))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"user_id": user.ID})
}

func (s *Server) loginUser(want string) (config.UserConfig, bool) {
	want = strings.TrimSpace(want)
	if want != "" {
		for _, u := range s.cfg.Users {
			if strings.EqualFold(u.ID, want) {
				return u, true
			}
		}
		return config.UserConfig{}, false
	}
	if len(s.cfg.Users) == 1 {
		return s.cfg.Users[0], true
	}
	var operators []config.UserConfig
	for _, u := range s.cfg.Users {
		for _, role := range u.Roles {
			if strings.EqualFold(role, "operator") {
				operators = append(operators, u)
				break
			}
		}
	}
	if len(operators) == 1 {
		return operators[0], true
	}
	return config.UserConfig{}, false
}

func (s *loginStore) issueCode(user config.UserConfig, scope string, ttl time.Duration) (string, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(LoginCodeDigits)), nil))
	if err != nil {
		return "", fmt.Errorf("mint sign-in code: %w", err)
	}
	code := fmt.Sprintf("%0*d", LoginCodeDigits, n)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[code] = loginCode{
		UserID:    user.ID,
		Roles:     append([]string(nil), user.Roles...),
		Scope:     scope,
		ExpiresAt: time.Now().Add(ttl),
	}
	return code, nil
}

func (s *loginStore) consumeCode(got string) (userID string, roles []string, scope string, ok bool) {
	got = digitsOnly(got)
	if got == "" {
		return "", nil, "", false
	}
	if len(got) != LoginCodeDigits {
		return "", nil, "", false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for stored, rec := range s.codes {
		if subtle.ConstantTimeCompare([]byte(stored), []byte(got)) != 1 {
			continue
		}
		delete(s.codes, stored)
		if now.After(rec.ExpiresAt) {
			return "", nil, "", false
		}
		return rec.UserID, rec.Roles, rec.Scope, true
	}
	return "", nil, "", false
}

func requestIsLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
