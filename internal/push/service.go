// Package push delivers selective browser notifications. Device subscriptions
// and VAPID credentials belong to the web node and never enter agent context.
package push

import (
	"context"
	"crypto/elliptic"
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
)

type Event struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	Icon      string    `json:"icon"`
	At        time.Time `json:"-"`
	Attention bool      `json:"-"`
	Expires   time.Time `json:"-"`
}

type Device struct {
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
type Source func(context.Context, string) ([]Event, error)

type Service struct {
	mu     sync.Mutex
	data   snapshot
	path   string
	client *http.Client
	send   Sender
	retry  map[string]time.Time
}

func Open(path string, client *http.Client) (*Service, error) {
	if path == "" {
		return nil, errors.New("push: persistent storage required")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	copyClient := *client
	copyClient.Timeout = 10 * time.Second
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	s := &Service{path: path, client: &copyClient, send: webpush.SendNotificationWithContext, retry: map[string]time.Time{}}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(raw, &s.data); err != nil {
			return nil, err
		}
		if s.data.PrivateKey == "" || s.data.PublicKey == "" || s.data.Devices == nil || s.data.Sent == nil {
			return nil, errors.New("push: invalid saved state")
		}
	} else if os.IsNotExist(err) {
		private, public, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			return nil, err
		}
		s.data = snapshot{PrivateKey: private, PublicKey: public, Devices: map[string]Device{}, Sent: map[string]time.Time{}}
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	} else {
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
	x, _ := elliptic.Unmarshal(elliptic.P256(), key)
	if x == nil {
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
	if existed {
		since = old.Since
	}
	s.data.Devices[id] = Device{Owner: owner, Login: hash(login), Subscription: sub, Since: since, Expires: expires}
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
	s.mu.Lock()
	devices := map[string]Device{}
	for id, d := range s.data.Devices {
		if time.Now().Before(d.Expires) {
			devices[id] = d
		}
	}
	public, private := s.data.PublicKey, s.data.PrivateKey
	s.mu.Unlock()
	events := map[string][]Event{}
	var errs []error
	for id, d := range devices {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if _, ok := events[d.Owner]; !ok {
			got, err := source(ctx, d.Owner)
			if err != nil {
				errs = append(errs, err)
				events[d.Owner] = nil
				continue
			}
			events[d.Owner] = got
		}
		for _, event := range events[d.Owner] {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(errs, err)...)
			}
			if !event.Attention && event.At.Before(d.Since) || event.At.Before(time.Now().Add(-24*time.Hour)) {
				continue
			}
			ttl := 3600
			if !event.Expires.IsZero() {
				remaining := int(time.Until(event.Expires).Seconds())
				if remaining <= 0 {
					continue
				}
				if remaining < ttl {
					ttl = remaining
				}
			}
			key := id + ":" + event.ID
			s.mu.Lock()
			_, sent := s.data.Sent[key]
			retry := s.retry[key]
			_, active := s.data.Devices[id]
			s.mu.Unlock()
			if !active || sent || time.Now().Before(retry) {
				continue
			}
			payload, err := json.Marshal(event)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			response, err := s.send(ctx, payload, &d.Subscription, &webpush.Options{HTTPClient: s.client, Subscriber: "https://github.com/jmylchreest/lobslaw", VAPIDPublicKey: public, VAPIDPrivateKey: private, TTL: ttl, Topic: hash(event.ID)[:32]})
			status := 0
			if response != nil {
				status = response.StatusCode
				response.Body.Close()
			}
			s.mu.Lock()
			switch {
			case err == nil && status >= 200 && status < 300:
				s.data.Sent[key] = time.Now()
				delete(s.retry, key)
			case status == 404 || status == 410:
				delete(s.data.Devices, id)
			default:
				s.retry[key] = time.Now().Add(2 * time.Minute)
				errs = append(errs, fmt.Errorf("push delivery failed (status %d); retry pending", status))
			}
			if saveErr := s.saveLocked(); saveErr != nil {
				errs = append(errs, saveErr)
			}
			s.mu.Unlock()
		}
	}
	return errors.Join(errs...)
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
	f, err := os.CreateTemp(filepath.Dir(s.path), ".push-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}
