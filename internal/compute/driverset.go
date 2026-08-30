package compute

import (
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
)

// A DriverSet is the name → constructor table that turns
// `driver = "anthropic"` in config into a working client.
//
// Explicit rather than a package-level registry populated by init().
// The side-effect-import pattern (`_ ".../drivers/anthropic"`) works,
// but it makes the set of available drivers depend on which files
// happen to be imported, which is invisible at the call site and
// awkward in tests that want a set with only a mock in it. A value
// passed at wiring time says exactly what this node can talk to.
//
// It also has to live here rather than in a drivers package: driver
// implementations import compute for the request types, so compute
// cannot import them back. internal/node assembles the real set.
type DriverSet struct {
	chat      map[string]ChatDriverFactory
	speak     map[string]SpeakDriverFactory
	image     map[string]ImageDriverFactory
	job       map[string]JobDriverFactory
	vision    map[string]VisionDriverFactory
	audio     map[string]AudioDriverFactory
	embedding map[string]EmbeddingDriverFactory
	search    map[string]SearchDriverFactory
}

// ChatDriverConfig is what every chat driver is built from. Fields a
// particular driver does not use are ignored — the alternative is a
// per-driver config type, which puts the vendor's shape back into the
// wiring layer that this exists to keep clean.
type ChatDriverConfig struct {
	Endpoint   string
	Model      string
	Credential Credential
	HTTPClient *http.Client
	Logger     *slog.Logger

	// ServerTools are provider-executed tools merged into the wire
	// request. Not universal, but not vendor-specific either — several
	// providers offer them, and a driver that does not simply drops
	// them.
	ServerTools []ServerTool
}

// ChatDriverFactory builds one configured chat driver.
type ChatDriverFactory func(ChatDriverConfig) (ChatDriver, error)

// NewDriverSet returns an empty set. A node with no drivers registered
// can serve no providers, which is a boot-time configuration error
// rather than a silent degradation — see Chat.
func NewDriverSet() *DriverSet {
	return &DriverSet{chat: make(map[string]ChatDriverFactory)}
}

// RegisterChat adds a driver under name. This is the "one line" in
// "adding a driver is one file and one registry line".
//
// Overwrites a previous registration, so a test can substitute a mock
// for a real driver name without rebuilding the set.
func (s *DriverSet) RegisterChat(name string, f ChatDriverFactory) {
	s.chat[strings.ToLower(strings.TrimSpace(name))] = f
}

// Chat builds a driver by name.
//
// An unknown name is an error naming what IS available, because the
// realistic failure is a typo or a driver the operator expected to
// exist, and "unknown driver: anthropc" plus the list resolves both
// without a documentation trip.
func (s *DriverSet) Chat(name string, cfg ChatDriverConfig) (ChatDriver, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		key = DriverOpenAI
	}
	f, ok := s.chat[key]
	if !ok {
		return nil, fmt.Errorf("unknown chat driver %q; available: %s",
			name, strings.Join(s.ChatNames(), ", "))
	}
	return f(cfg)
}

// ChatNames lists registered drivers, sorted, for error messages and
// for `lobslaw doctor`.
func (s *DriverSet) ChatNames() []string {
	out := make([]string, 0, len(s.chat))
	for k := range s.chat {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Driver names. Config uses these strings, so they are constants
// rather than literals scattered across the wiring.
const (
	// DriverOpenAI is the OpenAI-compatible wire format, and the
	// default when config names no driver. Most vendors speak it,
	// which is why "a custom variant is a different endpoint" holds
	// for chat even though it does not hold for generation.
	DriverOpenAI = "openai"

	// DriverAnthropic is the Messages API.
	DriverAnthropic = "anthropic"

	// DriverMock serves scripted responses and touches no network. A
	// node whose providers are all mock drivers boots and serves a
	// full turn offline, which is what the end-to-end harness needs.
	DriverMock = "mock"

	// DriverChatMultimodal is audio-on-chat: /v1/chat/completions with
	// an input_audio content part, as OpenRouter exposes it. Named for
	// the SHAPE rather than the vendor, because several providers offer
	// it and none of them owns it.
	DriverChatMultimodal = "openrouter"

	// DriverMiniMax is MiniMax's embedding shape: {texts, model, type}
	// returning {vectors}, and reporting failure in a body field on an
	// HTTP 200.
	DriverMiniMax = "minimax"
)

// MockChatFactory builds a mock chat driver from config.
//
// The reply names the configured model and the number of messages the
// request carried, and both are load-bearing for tests that cannot
// reach inside the node:
//
//   - the model name shows WHICH provider answered, which is what a
//     failover test needs — a mock that always says the same thing
//     cannot show that the backup replied;
//   - the message count shows how much history was replayed, which
//     lets an end-to-end test prove a transcript round-tripped
//     through raft using only the HTTP surface, rather than by
//     reaching into the store and asserting on its internals.
func MockChatFactory(cfg ChatDriverConfig) (ChatDriver, error) {
	model := cfg.Model
	if model == "" {
		model = "mock"
	}
	return NewMockProviderFunc(func(req ChatRequest, _ int) (MockResponse, error) {
		return MockResponse{
			Content:      fmt.Sprintf("mock reply from %s (saw %d messages)", model, len(req.Messages)),
			FinishReason: "stop",
			Usage:        Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		}, nil
	}), nil
}

// OpenAIChatFactory adapts the existing OpenAI-compatible client.
//
// It takes an API key rather than a Credential because that client
// predates the Credential type; the adaptation happens here so the
// wiring layer only ever deals in Credentials. When the client is
// rewritten to take one directly this shim goes away.
func OpenAIChatFactory(cfg ChatDriverConfig) (ChatDriver, error) {
	var apiKey string
	if sc, ok := cfg.Credential.(*StaticCredential); ok {
		apiKey = sc.Value
	}
	return NewLLMClient(LLMClientConfig{
		Endpoint:    cfg.Endpoint,
		APIKey:      apiKey,
		Model:       cfg.Model,
		ServerTools: cfg.ServerTools,
		Logger:      cfg.Logger,
	})
}

// --- generation modalities -------------------------------------------
//
// Chat resolved its driver by name from the start; the generation
// modalities did not, and each hardcoded one constructor. That made a
// second vendor a rewrite of the wiring rather than a registration,
// which is the opposite of what the waist is for.
//
// One config type per modality rather than one shared type: a speak
// request has a voice and a video request has a poll cadence, and
// collapsing them would put every vendor's shape back into a single
// struct that the wiring layer has to understand.

// SpeakDriverConfig is what every speak driver is built from.
type SpeakDriverConfig struct {
	Endpoint   string
	Model      string
	Voice      string
	Format     string
	Credential Credential
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// ImageDriverConfig is what every image driver is built from.
type ImageDriverConfig struct {
	Endpoint   string
	Model      string
	Size       string
	Quality    string
	Credential Credential
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// JobDriverConfig is what every asynchronous driver is built from.
// Endpoint is the submit URL; a driver needing a second URL to poll
// derives it or takes its own default, because the relationship
// between the two is vendor-specific and not expressible here.
type JobDriverConfig struct {
	Endpoint   string
	Model      string
	Credential Credential
	HTTPClient *http.Client
	Logger     *slog.Logger
}

type (
	SpeakDriverFactory func(SpeakDriverConfig) (SpeakDriver, error)
	ImageDriverFactory func(ImageDriverConfig) (ImageDriver, error)
	JobDriverFactory   func(JobDriverConfig) (JobDriver, error)
)

func (s *DriverSet) RegisterSpeak(name string, f SpeakDriverFactory) {
	if s.speak == nil {
		s.speak = map[string]SpeakDriverFactory{}
	}
	s.speak[normaliseDriverName(name)] = f
}

func (s *DriverSet) RegisterImage(name string, f ImageDriverFactory) {
	if s.image == nil {
		s.image = map[string]ImageDriverFactory{}
	}
	s.image[normaliseDriverName(name)] = f
}

func (s *DriverSet) RegisterJob(name string, f JobDriverFactory) {
	if s.job == nil {
		s.job = map[string]JobDriverFactory{}
	}
	s.job[normaliseDriverName(name)] = f
}

// Speak builds a speak driver by name. An empty name picks the
// OpenAI-compatible shape, which keeps configs that predate driver
// selection working.
func (s *DriverSet) Speak(name string, cfg SpeakDriverConfig) (SpeakDriver, error) {
	key := normaliseDriverName(name)
	if key == "" {
		key = DriverOpenAI
	}
	f, ok := s.speak[key]
	if !ok {
		return nil, fmt.Errorf("unknown speak driver %q; available: %s",
			name, strings.Join(slices.Sorted(maps.Keys(s.speak)), ", "))
	}
	return f(cfg)
}

// Image builds an image driver by name.
func (s *DriverSet) Image(name string, cfg ImageDriverConfig) (ImageDriver, error) {
	key := normaliseDriverName(name)
	if key == "" {
		key = DriverOpenAI
	}
	f, ok := s.image[key]
	if !ok {
		return nil, fmt.Errorf("unknown image driver %q; available: %s",
			name, strings.Join(slices.Sorted(maps.Keys(s.image)), ", "))
	}
	return f(cfg)
}

// Job builds an asynchronous driver by name.
//
// Unlike chat, speak and image there is no default: the three async
// protocols share nothing, so "whatever the default is" would be a
// guess that fails at submit rather than at boot.
func (s *DriverSet) Job(name string, cfg JobDriverConfig) (JobDriver, error) {
	key := normaliseDriverName(name)
	if key == "" {
		return nil, fmt.Errorf("a video provider must name its driver explicitly (available: %s); "+
			"the async protocols share no common shape, so there is no sensible default",
			strings.Join(slices.Sorted(maps.Keys(s.job)), ", "))
	}
	f, ok := s.job[key]
	if !ok {
		return nil, fmt.Errorf("unknown job driver %q; available: %s",
			name, strings.Join(slices.Sorted(maps.Keys(s.job)), ", "))
	}
	return f(cfg)
}

func normaliseDriverName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// OpenAISpeakFactory adapts the /v1/audio/speech driver.
func OpenAISpeakFactory(cfg SpeakDriverConfig) (SpeakDriver, error) {
	return NewOpenAISpeakDriver(OpenAISpeakConfig{
		Endpoint:   cfg.Endpoint,
		Model:      cfg.Model,
		Voice:      cfg.Voice,
		Format:     cfg.Format,
		Credential: cfg.Credential,
		HTTPClient: cfg.HTTPClient,
	})
}

// OpenAIImageFactory adapts the /v1/images/generations driver.
func OpenAIImageFactory(cfg ImageDriverConfig) (ImageDriver, error) {
	return NewOpenAIImageDriver(OpenAIImageConfig{
		Endpoint:   cfg.Endpoint,
		Model:      cfg.Model,
		Size:       cfg.Size,
		Quality:    cfg.Quality,
		Credential: cfg.Credential,
		HTTPClient: cfg.HTTPClient,
	})
}

// MockJobFactory registers the offline job driver, so a node can be
// configured for video generation with no vendor account and still
// exercise submit, commitment, poll and delivery end to end.
func MockJobFactory(JobDriverConfig) (JobDriver, error) {
	return &MockJobDriver{PollsBeforeDone: 1}, nil
}
