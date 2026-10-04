package calendar

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/httpbody"
	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

const (
	tokenEndpoint         = "https://oauth2.googleapis.com/token"
	userInfoEndpoint      = "https://openidconnect.googleapis.com/v1/userinfo"
	authorizationEndpoint = "https://accounts.google.com/o/oauth2/v2/auth"
	cookieName            = "__Host-lobslaw-google"
)

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
}
type flow struct {
	Stage      string         `json:"stage"`
	Write      bool           `json:"write"`
	Verifier   string         `json:"verifier"`
	CookieHash string         `json:"cookie_hash"`
	Token      *tokenResponse `json:"token,omitempty"`
	ExpiresAt  time.Time      `json:"expires_at"`
	Subject    string         `json:"subject"`
	Email      string         `json:"email"`
}
type ConnectIntent struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Service) Begin(ctx context.Context, write bool) (ConnectIntent, error) {
	var out ConnectIntent
	if err := s.authorize(ctx, "calendar:connect", "google"); err != nil {
		return out, err
	}
	owner, _ := principal(ctx)
	id := randomID()
	expiry := time.Now().Add(stateTTL)
	f := flow{Stage: "new", Write: write, Verifier: randomID()}
	data, _ := json.Marshal(f)
	rec := &lobslawv1.IntegrationStateRecord{Id: id, Owner: owner, Kind: "google-oauth", Data: data, ExpiresAt: timestamppb.New(expiry)}
	if err := s.cfg.State.Save(ctx, rec); err != nil {
		return out, err
	}
	u, _ := url.Parse(s.cfg.RedirectURL)
	u.Path = "/integrations/google/start"
	u.RawQuery = url.Values{"intent": {id}}.Encode()
	return ConnectIntent{ID: id, URL: u.String(), ExpiresAt: expiry}, nil
}
func (s *Service) loadFlow(ctx context.Context, id string) (*lobslawv1.IntegrationStateRecord, flow, error) {
	var f flow
	if len(id) != 43 {
		return nil, f, errors.New("calendar: invalid connection attempt")
	}
	r, err := s.cfg.State.Get(ctx, id)
	if err != nil {
		return nil, f, err
	}
	if r.Kind != "google-oauth" || r.ExpiresAt == nil || !r.ExpiresAt.AsTime().After(time.Now()) {
		return nil, f, errors.New("calendar: connection attempt expired")
	}
	err = json.Unmarshal(r.Data, &f)
	return r, f, err
}
func (s *Service) saveFlow(ctx context.Context, r *lobslawv1.IntegrationStateRecord, f flow) error {
	r.Data, _ = json.Marshal(f)
	return s.cfg.State.Save(ctx, r)
}
func (s *Service) tokenRequest(ctx context.Context, form url.Values) (tokenResponse, error) {
	var tok tokenResponse
	form.Set("client_id", s.cfg.ClientID)
	form.Set("client_secret", s.cfg.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tok, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return tok, errors.New("calendar: Google authorization request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := httpbody.Read(resp.Body, responseLimit)
	if err != nil {
		return tok, errors.New("calendar: invalid authorization response")
	}
	if resp.StatusCode != http.StatusOK {
		return tok, errors.New("calendar: Google authorization rejected; reconnect")
	}
	if json.Unmarshal(body, &tok) != nil || tok.AccessToken == "" || tok.ExpiresIn <= 0 || !strings.EqualFold(tok.TokenType, "bearer") {
		return tok, errors.New("calendar: incomplete authorization response")
	}
	return tok, nil
}
func (s *Service) refresh(ctx context.Context, token string) (string, string, int, string, error) {
	tok, err := s.tokenRequest(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token}})
	return tok.AccessToken, tok.RefreshToken, tok.ExpiresIn, tok.Scope, err
}

// BrowserHandler exposes only the OAuth start/callback surface, not agent APIs.
func (s *Service) BrowserHandler() http.Handler { return http.HandlerFunc(s.browser) }
func (s *Service) browser(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var err error
	switch r.URL.Path {
	case "/integrations/google/start":
		err = s.startBrowser(w, r)
	case "/integrations/google/callback":
		err = s.callback(w, r)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Google connection failed or expired. Start a new connection from your private lobslaw session.", http.StatusBadRequest)
	}
}
func (s *Service) startBrowser(w http.ResponseWriter, r *http.Request) error {
	rec, f, err := s.loadFlow(r.Context(), r.URL.Query().Get("intent"))
	if err != nil {
		return err
	}
	if f.Stage != "new" {
		return errors.New("already started")
	}
	cookie := randomID()
	sum := sha256.Sum256([]byte(cookie))
	f.CookieHash = base64.RawURLEncoding.EncodeToString(sum[:])
	f.Stage = "browser"
	if err := s.saveFlow(r.Context(), rec, f); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: cookie, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(stateTTL.Seconds())})
	scopes := []string{"openid", "email", scopeCalendarList, scopeEventsRead}
	if f.Write {
		scopes[len(scopes)-1] = scopeEventsWrite
	}
	challenge := sha256.Sum256([]byte(f.Verifier))
	values := url.Values{"client_id": {s.cfg.ClientID}, "redirect_uri": {s.cfg.RedirectURL}, "response_type": {"code"}, "scope": {strings.Join(scopes, " ")}, "state": {rec.Id}, "access_type": {"offline"}, "prompt": {"consent select_account"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}}
	http.Redirect(w, r, authorizationEndpoint+"?"+values.Encode(), http.StatusSeeOther)
	return nil
}
func (s *Service) callback(w http.ResponseWriter, r *http.Request) error {
	rec, f, err := s.loadFlow(r.Context(), r.URL.Query().Get("state"))
	if err != nil {
		return err
	}
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(cookie.Value))
	hash := base64.RawURLEncoding.EncodeToString(sum[:])
	if f.Stage != "browser" || subtle.ConstantTimeCompare([]byte(hash), []byte(f.CookieHash)) != 1 {
		return errors.New("callback binding mismatch")
	}
	code := r.URL.Query().Get("code")
	if code == "" || len(code) > 8192 || r.URL.Query().Get("error") != "" {
		return errors.New("authorization denied")
	}
	f.Stage = "exchanging"
	if err := s.saveFlow(r.Context(), rec, f); err != nil {
		return err
	}
	rec.Revision++
	tok, err := s.tokenRequest(r.Context(), url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {f.Verifier}, "redirect_uri": {s.cfg.RedirectURL}})
	if err != nil {
		return err
	}
	granted := strings.Fields(tok.Scope)
	if tok.RefreshToken == "" || !slices.Contains(granted, scopeCalendarList) || (!slices.Contains(granted, scopeEventsRead) && !slices.Contains(granted, scopeEventsWrite)) {
		return errors.New("required permission missing")
	}
	if f.Write && !slices.Contains(granted, scopeEventsWrite) {
		return errors.New("write permission missing")
	}
	var who struct {
		Subject  string `json:"sub"`
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
	}
	if err := s.request(r.Context(), http.MethodGet, userInfoEndpoint, tok.AccessToken, nil, "", &who); err != nil {
		return err
	}
	if who.Subject == "" || who.Email == "" || !who.Verified {
		return errors.New("account identity unavailable")
	}
	f.Stage = "pending"
	f.Verifier = ""
	f.CookieHash = ""
	f.Token = &tok
	f.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	f.Subject = who.Subject
	f.Email = who.Email
	if err := s.saveFlow(r.Context(), rec, f); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", Secure: true, HttpOnly: true, MaxAge: -1, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, err = w.Write([]byte("Google consent received. Return to your private lobslaw session to review the account and select calendars. No calendar access is active yet."))
	return err
}

type PendingView struct {
	ID        string         `json:"id"`
	Email     string         `json:"email"`
	Calendars []CalendarView `json:"calendars"`
	Write     bool           `json:"write"`
}

func (s *Service) Pending(ctx context.Context, id string) (PendingView, error) {
	var out PendingView
	if err := s.authorize(ctx, "calendar:connect", "google"); err != nil {
		return out, err
	}
	owner, _ := principal(ctx)
	r, f, err := s.loadFlow(ctx, id)
	if err != nil {
		return out, err
	}
	if r.Owner != owner || f.Stage != "pending" || f.Token == nil {
		return out, errors.New("calendar: pending connection unavailable")
	}
	var page struct {
		Items         []CalendarView `json:"items"`
		NextPageToken string         `json:"nextPageToken"`
	}
	if err := s.request(ctx, http.MethodGet, apiBase+"/users/me/calendarList?maxResults=250", f.Token.AccessToken, nil, "", &page); err != nil {
		return out, err
	}
	if page.NextPageToken != "" {
		return out, errors.New("calendar: too many calendars for initial connection; reduce subscribed calendars")
	}
	return PendingView{ID: id, Email: f.Email, Calendars: page.Items, Write: f.Write}, nil
}

// Activate is a human account-management operation, never an agent tool.
func (s *Service) Activate(ctx context.Context, id, email string, calendars map[string]Permission) (ConnectionView, error) {
	var out ConnectionView
	view, err := s.Pending(ctx, id)
	if err != nil {
		return out, err
	}
	if email != view.Email || len(calendars) == 0 || len(calendars) > 100 {
		return out, errors.New("calendar: confirm the displayed account and select calendars")
	}
	for id, permission := range calendars {
		found := false
		for _, cal := range view.Calendars {
			if cal.ID == id {
				found = (!permission.Write || (view.Write && (cal.AccessRole == "owner" || cal.AccessRole == "writer"))) && (permission.Read || permission.Write)
			}
		}
		if !found {
			return out, errors.New("calendar: selected calendar or permission unavailable")
		}
	}
	r, f, err := s.loadFlow(ctx, id)
	if err != nil {
		return out, err
	}
	owner, _ := principal(ctx)
	if r.Owner != owner || f.Stage != "pending" || f.Token == nil {
		return out, errors.New("calendar: pending connection unavailable")
	}
	f.Stage = "activating"
	if err := s.saveFlow(ctx, r, f); err != nil {
		return out, err
	}
	r.Revision++
	connection := randomID()
	names := map[string]string{}
	for _, cal := range view.Calendars {
		if _, ok := calendars[cal.ID]; ok {
			names[cal.ID] = cal.Name
		}
	}
	metadata, _ := json.Marshal(Connection{Names: names, Subject: f.Subject, Email: f.Email, Active: true, Calendars: calendars})
	p := &memory.PlaintextCredential{ID: connection, Provider: "google", Subject: connection, AccessToken: f.Token.AccessToken, RefreshToken: f.Token.RefreshToken, Scopes: strings.Fields(f.Token.Scope), ExpiresAt: f.ExpiresAt, ConnectorData: metadata}
	if err := s.cfg.Credentials.Put(ctx, p); err != nil {
		return out, err
	}
	f.Stage = "done"
	f.Token = nil
	if err := s.saveFlow(ctx, r, f); err != nil {
		return out, errors.New("calendar: connected but cleanup failed; list connections before reconnecting")
	}
	return ConnectionView{ID: connection, Email: f.Email, Calendars: calendars}, nil
}
func (s *Service) Connections(ctx context.Context) ([]ConnectionView, error) {
	if err := s.authorize(ctx, "calendar:connect", "google"); err != nil {
		return nil, err
	}
	profile, record, err := s.profile(ctx)
	if err != nil {
		return nil, err
	}
	inventory, err := s.inventory(ctx, profile, record.Revision)
	if err != nil {
		return nil, err
	}
	out := []ConnectionView{}
	for _, c := range inventory.Calendars {
		index := -1
		for i := range out {
			if out[i].ID == c.Connection {
				index = i
				break
			}
		}
		if index < 0 {
			out = append(out, ConnectionView{ID: c.Connection, Email: c.Account, Calendars: map[string]Permission{}})
			index = len(out) - 1
		}
		out[index].Calendars[c.Calendar] = c.Permission
	}
	return out, nil
}
func (s *Service) Disconnect(ctx context.Context, id string) error {
	if err := s.authorize(ctx, "calendar:connect", "google"); err != nil {
		return err
	}
	return s.cfg.Credentials.Delete(ctx, "google", id)
}
