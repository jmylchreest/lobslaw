// Package push delivers selective browser notifications. Device subscriptions
// and VAPID credentials belong to the web node and never enter agent context.
package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/jmylchreest/lobslaw/internal/atomicfile"
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/notify"
)

type Event = notify.Event

type Device struct {
	Generation   string
	Owner        string
	Login        string
	Subscription webpush.Subscription
	Since        time.Time
	Expires      time.Time
}

type snapshot struct {
	PrivateKey string
	PublicKey  string
	Devices    map[string]Device
	Sent       map[string]time.Time
}

type Sender func(context.Context, []byte, *webpush.Subscription, *webpush.Options) (*http.Response, error)
type Source = notify.EventSource

type Service struct {
	// Only one outbox scan runs at a time. Subscription mutation remains
	// independent of network I/O; both sends and acknowledgements check generation.
	dispatchMu sync.Mutex
	mu         sync.Mutex
	data       snapshot
	path       string
	client     *http.Client
	send       Sender
	retry      map[string]time.Time
}

func Open(path string, client *http.Client) (*Service, error) {
	if path == "" {
		return nil, errors.New("push: persistent storage required")
	}
	if client == nil {
		client = egress.For("gateway/web-push").HTTPClient()
	}
	copyClient := *client
	copyClient.Timeout = 10 * time.Second
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	s := &Service{path: path, client: &copyClient, send: webpush.SendNotificationWithContext, retry: map[string]time.Time{}}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s.data); err != nil {
			return nil, err
		}
		if s.data.PrivateKey == "" || s.data.PublicKey == "" || s.data.Devices == nil || s.data.Sent == nil {
			return nil, errors.New("push: invalid saved state")
		}
		// Old workers cannot enforce audience binding. Require those devices to
		// register through the versioned protocol before resuming private push.
		for id, d := range s.data.Devices {
			if d.Generation == "" {
				delete(s.data.Devices, id)
			}
		}
	case os.IsNotExist(err):
		private, public, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			return nil, err
		}
		s.data = snapshot{PrivateKey: private, PublicKey: public, Devices: map[string]Device{}, Sent: map[string]time.Time{}}
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	return s, nil
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func (s *Service) PublicKey() string { s.mu.Lock(); defer s.mu.Unlock(); return s.data.PublicKey }

func ValidateSubscription(sub webpush.Subscription) error {
	u, err := url.Parse(sub.Endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" || len(sub.Endpoint) > 2048 {
		return errors.New("invalid push endpoint")
	}
	host := strings.ToLower(u.Hostname())
	if host != "fcm.googleapis.com" && host != "updates.push.services.mozilla.com" && host != "web.push.apple.com" {
		return errors.New("unsupported browser push service")
	}
	key, err := base64.RawURLEncoding.DecodeString(sub.Keys.P256dh)
	if err != nil {
		return errors.New("invalid push public key")
	}
	if _, err := ecdh.P256().NewPublicKey(key); err != nil {
		return errors.New("invalid push public key")
	}
	auth, err := base64.RawURLEncoding.DecodeString(sub.Keys.Auth)
	if err != nil || len(auth) != 16 {
		return errors.New("invalid push auth key")
	}
	return nil
}

func (s *Service) Subscribe(owner, login string, expires time.Time, sub webpush.Subscription) error {
	if err := ValidateSubscription(sub); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := hash(sub.Endpoint)
	old, existed := s.data.Devices[id]
	if existed && !time.Now().Before(old.Expires) {
		delete(s.data.Devices, id)
		existed = false
	}
	if existed && old.Owner != owner {
		return errors.New("subscription belongs to another account; unsubscribe in this browser first")
	}
	count := 0
	for _, device := range s.data.Devices {
		if device.Owner == owner && time.Now().Before(device.Expires) {
			count++
		}
	}
	if !existed && count >= 10 {
		return errors.New("maximum ten devices per user")
	}
	since := time.Now()
	generation := old.Generation
	if existed && old.Login == hash(login) && old.Subscription.Keys == sub.Keys {
		since = old.Since
	} else {
		var secret [16]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return err
		}
		generation = hex.EncodeToString(secret[:])
	}
	s.data.Devices[id] = Device{Generation: generation, Owner: owner, Login: hash(login), Subscription: sub, Since: since, Expires: expires}
	if err := s.saveLocked(); err != nil {
		if existed {
			s.data.Devices[id] = old
		} else {
			delete(s.data.Devices, id)
		}
		return err
	}
	return nil
}

func (s *Service) Remove(owner, endpoint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := hash(endpoint)
	old, ok := s.data.Devices[id]
	if !ok || old.Owner != owner {
		return nil
	}
	delete(s.data.Devices, id)
	if err := s.saveLocked(); err != nil {
		s.data.Devices[id] = old
		return err
	}
	return nil
}

func (s *Service) RemoveLogin(login string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := map[string]Device{}
	for id, d := range s.data.Devices {
		if d.Login == hash(login) {
			removed[id] = d
			delete(s.data.Devices, id)
		}
	}
	if err := s.saveLocked(); err != nil {
		for id, d := range removed {
			s.data.Devices[id] = d
		}
		return err
	}
	return nil
}

// Dispatch polls durable evidence. Success receipts survive restart; transient
// failures retry with backoff. A stable browser tag coalesces ambiguous retries.
func (s *Service) Dispatch(ctx context.Context, source Source) error {
	return s.DispatchPages(ctx, notify.SinglePage(source))
}

// DispatchPages streams durable candidates without retaining an owner's entire
// outbox in memory. A partial scan leaves successful delivery receipts intact.
func (s *Service) DispatchPages(ctx context.Context, source notify.EventPages) error {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	s.mu.Lock()
	owners := map[string]map[string]Device{}
	for id, d := range s.data.Devices {
		if time.Now().Before(d.Expires) {
			if owners[d.Owner] == nil {
				owners[d.Owner] = map[string]Device{}
			}
			owners[d.Owner][id] = d
		}
	}
	s.mu.Unlock()
	var errs []error
	for owner, devices := range owners {
		for cursor := ""; ; {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(errs, err)...)
			}
			page, err := source(ctx, owner, cursor)
			if err != nil {
				errs = append(errs, err)
				break
			}
			for _, event := range page.Events {
				for id, d := range devices {
					if err := s.deliver(ctx, id, d, event); err != nil {
						errs = append(errs, err)
					}
				}
			}
			if page.Next == "" {
				break
			}
			if page.Next == cursor {
				errs = append(errs, errors.New("notification cursor did not advance"))
				break
			}
			cursor = page.Next
		}
	}
	return errors.Join(errs...)
}

func (s *Service) deliver(ctx context.Context, id string, d Device, event Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !event.Attention && event.At.Before(d.Since) || event.At.Before(time.Now().Add(-24*time.Hour)) {
		return nil
	}
	ttl := 3600
	if !event.Expires.IsZero() {
		ttl = min(ttl, int(time.Until(event.Expires).Seconds()))
		if ttl <= 0 {
			return nil
		}
	}
	key := id + ":" + d.Generation + ":" + event.ID
	s.mu.Lock()
	current, active := s.data.Devices[id]
	_, sent := s.data.Sent[key]
	retry := s.retry[key]
	public, private := s.data.PublicKey, s.data.PrivateKey
	s.mu.Unlock()
	if !active || current.Generation != d.Generation || current.Owner != d.Owner || current.Login != d.Login || !time.Now().Before(current.Expires) || sent || time.Now().Before(retry) {
		return nil
	}
	payload, err := json.Marshal(struct {
		Event
		Audience string `json:"audience"`
	}{event, d.Generation})
	if err != nil {
		return err
	}
	response, sendErr := s.send(ctx, payload, &d.Subscription, &webpush.Options{HTTPClient: s.client, Subscriber: "https://github.com/jmylchreest/lobslaw", VAPIDPublicKey: public, VAPIDPrivateKey: private, TTL: ttl, Topic: hash(event.ID)[:32]})
	status := 0
	if response != nil {
		status = response.StatusCode
		_ = response.Body.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, active = s.data.Devices[id]
	if !active || current.Generation != d.Generation || current.Owner != d.Owner || current.Login != d.Login {
		return nil
	}
	var deliveryErr error
	switch {
	case sendErr == nil && status >= 200 && status < 300:
		s.data.Sent[key] = time.Now()
		delete(s.retry, key)
	case status == 404 || status == 410:
		delete(s.data.Devices, id)
	default:
		s.retry[key] = time.Now().Add(2 * time.Minute)
		deliveryErr = fmt.Errorf("push delivery failed (status %d); retry pending", status)
	}
	return errors.Join(deliveryErr, s.saveLocked())
}

func (s *Service) saveLocked() error {
	for key, at := range s.data.Sent {
		if at.Before(time.Now().Add(-31 * 24 * time.Hour)) {
			delete(s.data.Sent, key)
		}
	}
	for id, d := range s.data.Devices {
		if !time.Now().Before(d.Expires) {
			delete(s.data.Devices, id)
		}
	}
	raw, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	return atomicfile.WritePrivate(s.path, raw)
}

// Binding is an opaque audience identifier, not an authentication credential.
func (s *Service) Binding(owner, login, endpoint string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.data.Devices[hash(endpoint)]
	if !ok || d.Owner != owner || d.Login != hash(login) || !time.Now().Before(d.Expires) {
		return ""
	}
	return d.Generation
}
