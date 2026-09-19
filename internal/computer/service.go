// Package computer owns private, local browser execution state. Authoritative
// project ownership and routine approval remain in the workforce service.
package computer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ActionTimeout   time.Duration = 30 * time.Second
	MaxWorkspaces   int           = 4
	MaxSteps        int           = 100
	privateDirMode  os.FileMode   = 0700
	privateFileMode os.FileMode   = 0600
)

var (
	ErrForbidden   = errors.New("computer: project access denied")
	ErrNotFound    = errors.New("computer: project not found")
	ErrUnavailable = errors.New("computer: unavailable; configure computer runtime, private root, and egress UDS")
	ErrTakeover    = errors.New("computer: human has control; return control before resuming the task")
	ErrManual      = errors.New("computer: sensitive step requires human entry and explicit task resume")
	ErrConflict    = errors.New("computer: take control before interacting or recording")
	ErrInvalid     = errors.New("computer: invalid action")
)

type ProjectAuthorizer interface {
	AuthorizeProject(ctx context.Context, principal, projectID string) error
}

type RoutineStep struct {
	Action      string   `json:"action"`
	Selector    string   `json:"selector,omitempty"`
	Value       string   `json:"value,omitempty"`
	URL         string   `json:"url,omitempty"`
	Description string   `json:"description,omitempty"`
	Sensitive   bool     `json:"sensitive,omitempty"`
	X           *float64 `json:"x,omitempty"`
	Y           *float64 `json:"y,omitempty"`
}

type State struct {
	ProjectID string        `json:"project_id"`
	Available bool          `json:"available"`
	Control   string        `json:"control"`
	Recording bool          `json:"recording"`
	Steps     []RoutineStep `json:"steps"`
	UpdatedAt time.Time     `json:"updated_at"`
}

type Result struct {
	Screenshot string `json:"screenshot,omitempty"`
	Selector   string `json:"selector,omitempty"`
}

type browser interface {
	Do(context.Context, RoutineStep) (Result, error)
	Close() error
}

type workspace struct {
	gate    chan struct{}
	path    string
	state   State
	browser browser
	users   int
}

type Service struct {
	cfg      Config
	auth     ProjectAuthorizer
	mu       sync.Mutex
	spaces   map[string]*workspace
	closed   atomic.Bool
	rootLock *os.File
	open     func(context.Context, string) (browser, error)
}

func New(cfg Config, auth ProjectAuthorizer) *Service {
	s := &Service{cfg: cfg, auth: auth, spaces: make(map[string]*workspace)}
	s.open = cfg.openBrowser
	return s
}

func (s *Service) AuthorizeProject(ctx context.Context, principal, projectID string) error {
	if s == nil || s.auth == nil {
		return ErrUnavailable
	}
	if principal == "" || projectID == "" {
		return ErrForbidden
	}
	if err := s.auth.AuthorizeProject(ctx, principal, projectID); err != nil {
		return err
	}
	return nil
}

func (s *Service) get(ctx context.Context, principal, projectID string) (*workspace, error) {
	if err := s.AuthorizeProject(ctx, principal, projectID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() || s.cfg.Root == "" {
		return nil, ErrUnavailable
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(principal+"\x00"+projectID)))
	if w := s.spaces[key]; w != nil {
		w.users++
		return w, nil
	}
	if len(s.spaces) >= MaxWorkspaces {
		for key, w := range s.spaces {
			if w.users == 0 && w.browser == nil {
				delete(s.spaces, key)
				break
			}
		}
	}
	if len(s.spaces) >= MaxWorkspaces {
		return nil, fmt.Errorf("%w: workspace capacity reached; close an inactive browser first", ErrUnavailable)
	}
	path := filepath.Join(s.cfg.Root, key)
	if err := privateDirectory(s.cfg.Root); err != nil {
		return nil, err
	}
	if s.rootLock == nil {
		lock, err := lockRoot(s.cfg.Root)
		if err != nil {
			return nil, err
		}
		s.rootLock = lock
	}
	if err := privateDirectory(path); err != nil {
		return nil, err
	}
	w := &workspace{gate: make(chan struct{}, 1), path: path, state: State{ProjectID: projectID, Control: "bot", Steps: []RoutineStep{}}}
	data, err := os.ReadFile(filepath.Join(path, "control.json"))
	if err == nil {
		if err := json.Unmarshal(data, &w.state); err != nil {
			return nil, fmt.Errorf("computer control state: %w", err)
		}
		if w.state.Control != "bot" && w.state.Control != "human" {
			return nil, ErrConflict
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("computer control state: %w", err)
	}
	w.state.Available = false
	w.users = 1
	s.spaces[key] = w
	return w, nil
}

func (s *Service) release(w *workspace) { s.mu.Lock(); w.users--; s.mu.Unlock() }

func (w *workspace) lock(ctx context.Context) error {
	select {
	case w.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (w *workspace) unlock() { <-w.gate }

func privateDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%w: root must be absolute", ErrUnavailable)
	}
	if err := os.MkdirAll(path, privateDirMode); err != nil {
		return fmt.Errorf("computer private directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("computer private directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != privateDirMode {
		return fmt.Errorf("%w: %s must be a private non-symlink 0700 directory", ErrUnavailable, path)
	}
	return nil
}

func (w *workspace) save() error {
	data, err := json.Marshal(w.state)
	if err != nil {
		return fmt.Errorf("computer encode control: %w", err)
	}
	f, err := os.CreateTemp(w.path, ".control-")
	if err != nil {
		return fmt.Errorf("computer save control: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(w.path, "control.json"))
	}
	if err == nil {
		var dir *os.File
		dir, err = os.Open(w.path)
		if err == nil {
			err = dir.Sync()
			_ = dir.Close()
		}
	}
	if err != nil {
		return fmt.Errorf("computer save control: %w", err)
	}
	return nil
}

func (s *Service) State(ctx context.Context, principal, projectID string) (State, error) {
	w, err := s.get(ctx, principal, projectID)
	if err != nil {
		return State{}, err
	}
	defer s.release(w)
	if err := w.lock(ctx); err != nil {
		return State{}, err
	}
	defer w.unlock()
	state := w.state
	state.Steps = slices.Clone(state.Steps)
	return state, nil
}

func (s *Service) Action(ctx context.Context, principal, projectID string, step RoutineStep) (Result, error) {
	w, err := s.get(ctx, principal, projectID)
	if err != nil {
		return Result{}, err
	}
	defer s.release(w)
	if err := w.lock(ctx); err != nil {
		return Result{}, err
	}
	defer w.unlock()
	if s.closed.Load() {
		return Result{}, ErrUnavailable
	}
	switch step.Action {
	case "takeover", "release", "record", "discard":
		before := w.state
		w.state.UpdatedAt = time.Now().UTC()
		switch step.Action {
		case "takeover":
			w.state.Control = "human"
		case "release":
			w.state.Control = "bot"
			w.state.Recording = false
		case "record":
			if w.state.Control != "human" {
				return Result{}, ErrConflict
			}
			w.state.Recording = true
			w.state.Steps = []RoutineStep{}
		case "discard":
			w.state.Recording = false
			w.state.Steps = []RoutineStep{}
		}
		if err := w.save(); err != nil {
			w.state = before
			return Result{}, err
		}
		return Result{}, nil
	case "stop":
		if w.browser != nil {
			err = w.browser.Close()
			w.browser = nil
		}
		w.state.Available = false
		return Result{}, err
	case "start", "capture":
	default:
		if w.state.Control != "human" {
			return Result{}, ErrConflict
		}
	}
	result, err := s.perform(ctx, w, step)
	if err != nil {
		return Result{}, err
	}
	if w.state.Recording && step.Action != "start" && step.Action != "capture" {
		if len(w.state.Steps) >= MaxSteps {
			w.state.Recording = false
			_ = w.save()
			return result, fmt.Errorf("%w: recording is limited to %d steps", ErrConflict, MaxSteps)
		}
		if step.Action == "click" || step.Action == "wait" {
			step.X = nil
			step.Y = nil
			step.Selector = result.Selector
			if step.Selector == "" {
				step.Sensitive = true
			}
		}
		w.state.Steps = append(w.state.Steps, recordable(step))
		if err := w.save(); err != nil {
			return Result{}, err
		}
	}
	return result, nil
}

// ExecuteStep is intentionally separate from owner controls. The worker evaluates
// normal tool policy first, and cannot use takeover/release to bypass the fence.
func (s *Service) ExecuteStep(ctx context.Context, principal, projectID string, step RoutineStep) error {
	w, err := s.get(ctx, principal, projectID)
	if err != nil {
		return err
	}
	defer s.release(w)
	if err := w.lock(ctx); err != nil {
		return err
	}
	defer w.unlock()
	if w.state.Control == "human" {
		return ErrTakeover
	}
	if step.Sensitive {
		return ErrManual
	}
	if !browserAction(step.Action) {
		return ErrInvalid
	}
	_, err = s.perform(ctx, w, step)
	return err
}

func browserAction(action string) bool {
	return slices.Contains([]string{"navigate", "click", "fill", "press", "wait", "capture"}, action)
}

func validateStep(step RoutineStep) error {
	if (step.X == nil) != (step.Y == nil) {
		return ErrInvalid
	}
	if step.X != nil && (step.Action != "click" || *step.X < 0 || *step.X >= browserWidth || *step.Y < 0 || *step.Y >= browserHeight) {
		return ErrInvalid
	}
	if !browserAction(step.Action) && step.Action != "start" {
		return ErrInvalid
	}
	if step.Action == "navigate" {
		u, err := url.Parse(step.URL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
			return fmt.Errorf("%w: navigation requires http(s) without embedded credentials", ErrInvalid)
		}
	}
	return nil
}

func recordable(step RoutineStep) RoutineStep {
	step.Description = ""
	// Any typed value may be a credential, including ordinary text inputs and
	// contenteditable. Never guess based solely on type=password or field names.
	if step.Action == "fill" || step.Sensitive {
		return RoutineStep{Action: step.Action, Sensitive: true, Description: "Enter the value manually, then resume"}
	}
	if step.Action == "navigate" {
		u, err := url.Parse(step.URL)
		if err != nil || u.RawQuery != "" || u.Fragment != "" {
			return RoutineStep{Action: "navigate", Sensitive: true, Description: "Navigate manually, then resume"}
		}
	}
	return step
}

func (s *Service) perform(ctx context.Context, w *workspace, step RoutineStep) (Result, error) {
	if s.closed.Load() {
		return Result{}, ErrUnavailable
	}
	if err := validateStep(step); err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, ActionTimeout)
	defer cancel()
	if w.browser == nil {
		b, err := s.open(ctx, w.path)
		if err != nil {
			return Result{}, fmt.Errorf("%w: browser launch failed (%v)", ErrUnavailable, err)
		}
		w.browser = b
		w.state.Available = true
	}
	result, err := w.browser.Do(ctx, step)
	if err != nil {
		// Errors deliberately omit runtime diagnostics: Playwright includes input
		// values, page content and URLs in its ordinary timeout messages.
		_ = w.browser.Close()
		w.browser = nil
		w.state.Available = false
		return Result{}, fmt.Errorf("%w: browser action failed; reopen the workspace and check the selected element", ErrUnavailable)
	}
	return result, nil
}

func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed.Store(true)
	var errs []error
	for _, w := range s.spaces {
		_ = w.lock(context.Background())
		if w.browser != nil {
			errs = append(errs, w.browser.Close())
			w.browser = nil
		}
		w.unlock()
	}
	if s.rootLock != nil {
		errs = append(errs, s.rootLock.Close())
		s.rootLock = nil
	}
	return errors.Join(errs...)
}
