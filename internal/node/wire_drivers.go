package node

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/compute/drivers/anthropic"
	"github.com/jmylchreest/lobslaw/internal/compute/drivers/dashscope"
	"github.com/jmylchreest/lobslaw/internal/compute/drivers/elevenlabs"
	"github.com/jmylchreest/lobslaw/internal/compute/drivers/gemini"
	"github.com/jmylchreest/lobslaw/internal/compute/drivers/imagen"
	"github.com/jmylchreest/lobslaw/internal/compute/drivers/minimax"
	"github.com/jmylchreest/lobslaw/internal/compute/drivers/veo"
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

// The driver table for this build.
//
// Assembled here rather than in compute because driver packages import
// compute for the request types, so compute cannot import them back.
// That inversion is deliberate: compute owns the contract, the wiring
// layer owns which implementations exist.
//
// Adding a driver is one line below plus one package. If it ever needs
// more than that, the waist has sprung a leak.
// A package-level OnceValue rather than a method with a sync.Once
// beside it: the body never touches the receiver, so the set is a
// property of the BINARY — which drivers were compiled in — and not of
// any one node.
var driverSet = sync.OnceValue(func() *compute.DriverSet {
	s := compute.NewDriverSet()
	s.RegisterChat(compute.DriverOpenAI, compute.OpenAIChatFactory)
	s.RegisterChat(compute.DriverAnthropic, anthropicChatFactory)
	s.RegisterChat(compute.DriverMock, compute.MockChatFactory)
	// MiniMax and DashScope speak OpenAI-compatible CHAT and their
	// own image shape. Registering the vendor name for both means
	// `driver = "minimax"` selects the right wire format per
	// modality — which is what an operator naming a vendor means.
	//
	// Without these two lines a provider entry declared for image
	// generation fails BOOT on "unknown chat driver", because the
	// one driver field is consulted for every modality and the
	// chat lookup happens first.
	s.RegisterChat(minimax.DriverName, compute.OpenAIChatFactory)
	s.RegisterChat(dashscope.DriverName, compute.OpenAIChatFactory)

	// Generation modalities resolve their driver by name too, so a
	// second vendor is a registration rather than a rewrite of the
	// wiring. Job has no default registration under DriverOpenAI:
	// the async protocols share no shape, so there is nothing
	// sensible to default to.
	s.RegisterSpeak(compute.DriverOpenAI, compute.OpenAISpeakFactory)
	s.RegisterSpeak(elevenlabs.DriverName, elevenlabsSpeakFactory)
	s.RegisterSpeak(minimax.DriverName, minimaxSpeakFactory)
	s.RegisterSpeak(dashscope.DriverName, dashscopeSpeakFactory)
	s.RegisterSpeak(compute.DriverMock, compute.MockSpeakFactory)
	s.RegisterImage(compute.DriverOpenAI, compute.OpenAIImageFactory)
	s.RegisterImage(imagen.DriverName, imagenImageFactory)
	s.RegisterImage(minimax.DriverName, minimaxImageFactory)
	s.RegisterImage(dashscope.DriverName, dashscopeImageFactory)
	s.RegisterImage(compute.DriverMock, compute.MockImageFactory)
	s.RegisterJob(compute.DriverMock, compute.MockJobFactory)
	s.RegisterJob(dashscope.DriverName, dashscopeJobFactory)
	s.RegisterJob(veo.DriverName, veoJobFactory)

	// Vision joins the same seam: `driver` selects the wire shape
	// per modality, so a vendor's chat and vision protocols are
	// registered independently rather than implied by one another.
	s.RegisterVision(compute.DriverOpenAI, compute.OpenAIVisionFactory)
	s.RegisterVision(compute.DriverAnthropic, anthropic.VisionFactory)
	s.RegisterVision(gemini.DriverName, gemini.VisionFactory)
	s.RegisterVision(compute.DriverMock, compute.MockVisionFactory)

	// Audio picks its driver by matched CAPABILITY rather than by
	// the provider's `driver` key, so one chain can mix a Whisper
	// endpoint with a chat-multimodal one.
	s.RegisterAudio(compute.DriverOpenAI, compute.WhisperAudioFactory)
	s.RegisterAudio(compute.DriverChatMultimodal, compute.ChatMultimodalAudioFactory)
	s.RegisterAudio(compute.DriverMock, compute.MockAudioFactory)

	// Embeddings register a FACTORY rather than a built driver:
	// the client normalises the endpoint suffix before anything
	// can be built with it.
	s.RegisterEmbedding(compute.DriverOpenAI, compute.OpenAIEmbeddingFactory)
	s.RegisterEmbedding(compute.DriverMiniMax, compute.MiniMaxEmbeddingFactory)
	s.RegisterEmbedding(compute.DriverMock, compute.MockEmbeddingFactory)

	// Search is the shallowest modality, so it gets two tiers.
	// "exa" and "searxng" are compiled because they have real
	// behaviour behind them; "template" is the declarative
	// interpreter, and every other engine — Brave, Tavily, Serper,
	// a private proxy — is a TOML block against it rather than a
	// line here.
	s.RegisterSearch(compute.DriverExa, compute.ExaSearchFactory)
	s.RegisterSearch(compute.DriverKagi, compute.KagiSearchFactory)
	s.RegisterSearch(compute.DriverSearxng, compute.SearxngSearchFactory)
	s.RegisterSearch(compute.DriverTemplate, compute.TemplateSearchFactory)
	return s
})

func (n *Node) drivers() *compute.DriverSet { return driverSet() }

// anthropicChatFactory adapts the package's own constructor to the
// generic factory signature.
func anthropicChatFactory(cfg compute.ChatDriverConfig) (compute.ChatDriver, error) {
	return anthropic.New(anthropic.Config{
		Endpoint:   cfg.Endpoint,
		Model:      cfg.Model,
		Credential: cfg.Credential,
		HTTPClient: cfg.HTTPClient,
		Logger:     cfg.Logger,
	})
}

// credentialFor builds the credential a driver will apply.
//
// The header differs per protocol — Anthropic wants a bare x-api-key,
// everything else a bearer token — and that belongs here rather than
// in the driver, so a driver never has to ask what kind of credential
// it was handed. Vertex and Bedrock will add kinds here, not branches
// inside drivers.
func credentialFor(p config.ProviderConfig, apiKey string) compute.Credential {
	return credentialForDriver(p.Driver, apiKey)
}

// credentialForDriver picks the auth shape a driver expects. Split out
// from credentialFor because the generation modalities resolve their
// driver from an already-resolved endpoint rather than from the whole
// ProviderConfig, and both paths must agree on what "anthropic" means.
func credentialForDriver(driver, apiKey string) compute.Credential {
	if apiKey == "" {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case compute.DriverAnthropic:
		return compute.NewHeaderCredential("x-api-key", apiKey)
	case elevenlabs.DriverName:
		return compute.NewHeaderCredential("xi-api-key", apiKey)
	default:
		return compute.NewBearerCredential(apiKey)
	}
}

// dashscopeJobFactory adapts the Wan video driver to the registry.
func dashscopeJobFactory(cfg compute.JobDriverConfig) (compute.JobDriver, error) {
	return dashscope.New(dashscope.Config{
		SubmitEndpoint: cfg.Endpoint,
		Model:          cfg.Model,
		Credential:     cfg.Credential,
		HTTPClient:     cfg.HTTPClient,
	})
}

// elevenlabsSpeakFactory adapts the ElevenLabs driver.
func elevenlabsSpeakFactory(cfg compute.SpeakDriverConfig) (compute.SpeakDriver, error) {
	return elevenlabs.New(elevenlabs.Config{
		BaseURL:    cfg.Endpoint,
		Model:      cfg.Model,
		Voice:      cfg.Voice,
		Format:     cfg.Format,
		Credential: cfg.Credential,
		HTTPClient: cfg.HTTPClient,
	})
}

// veoJobFactory adapts the Vertex AI Veo driver.
func veoJobFactory(cfg compute.JobDriverConfig) (compute.JobDriver, error) {
	return veo.New(veo.Config{
		Endpoint:   cfg.Endpoint,
		Model:      cfg.Model,
		Credential: cfg.Credential,
		HTTPClient: cfg.HTTPClient,
	})
}

// imagenImageFactory adapts the Vertex AI Imagen driver.
func imagenImageFactory(cfg compute.ImageDriverConfig) (compute.ImageDriver, error) {
	return imagen.New(imagen.Config{
		Endpoint:   cfg.Endpoint,
		Model:      cfg.Model,
		Size:       cfg.Size,
		Credential: cfg.Credential,
		HTTPClient: cfg.HTTPClient,
	})
}

// minimaxImageFactory adapts MiniMax's image-generation endpoint.
func minimaxImageFactory(cfg compute.ImageDriverConfig) (compute.ImageDriver, error) {
	return minimax.NewImage(minimax.ImageConfig{
		Endpoint:   cfg.Endpoint,
		Model:      cfg.Model,
		Credential: cfg.Credential,
		HTTPClient: cfg.HTTPClient,
		Logger:     cfg.Logger,
	})
}

// dashscopeImageFactory adapts DashScope's synchronous
// multimodal-generation path — the Wan and Qwen-Image models.
func dashscopeImageFactory(cfg compute.ImageDriverConfig) (compute.ImageDriver, error) {
	return dashscope.NewImage(dashscope.ImageConfig{
		Endpoint:   cfg.Endpoint,
		Model:      cfg.Model,
		Credential: cfg.Credential,
		HTTPClient: cfg.HTTPClient,
		Logger:     cfg.Logger,
	})
}

// dashscopeSpeakFactory adapts Qwen TTS, which is a WebSocket task
// rather than a request — see the dashscope speak driver.
func dashscopeSpeakFactory(cfg compute.SpeakDriverConfig) (compute.SpeakDriver, error) {
	return dashscope.NewSpeak(dashscope.SpeakConfig{
		Endpoint:   cfg.Endpoint,
		Model:      cfg.Model,
		Voice:      cfg.Voice,
		Format:     cfg.Format,
		Credential: cfg.Credential,
		HTTPClient: cfg.HTTPClient,
		Logger:     cfg.Logger,
	})
}

// minimaxSpeakFactory adapts MiniMax's text-to-audio endpoint.
func minimaxSpeakFactory(cfg compute.SpeakDriverConfig) (compute.SpeakDriver, error) {
	return minimax.NewSpeak(minimax.SpeakConfig{
		Endpoint:   cfg.Endpoint,
		Model:      cfg.Model,
		Voice:      cfg.Voice,
		Format:     cfg.Format,
		Credential: cfg.Credential,
		HTTPClient: cfg.HTTPClient,
		Logger:     cfg.Logger,
	})
}

// modalityEgressTimeout is the ceiling on a modality driver's HTTP
// client when it is routed through the egress proxy.
//
// Deliberately LOOSER than every driver's own default (the longest is
// 3 minutes, for image generation) so that routing through the proxy
// cannot tighten a deadline the driver already chose. It is a ceiling
// for a driver that set none, not a policy about how long generation
// may take.
const modalityEgressTimeout = 5 * time.Minute

// modalityEgressClient returns the proxy-aware client for one
// provider's own egress role.
//
// Modality drivers were the only outbound path in the tree that did not
// route through smokescreen: chat, fetch_url, web_search, the gateways
// and the model download all did, while speak, image, video, vision and
// audio dialled their providers directly. The ACL already carried the
// hosts — "llm/<label>" is built for every [[compute.providers]] entry
// — so nothing needed declaring, only using.
//
// The client is COPIED before its timeout is set, as llmclient does:
// the provider hands out a shared client per role, and mutating it in
// place would change the deadline for every other caller of that role.
//
// egress.For's own DefaultTimeout is 30s, which is right for a chat
// round trip and wrong for image generation — passing that client
// unmodified is what turned "route this through the proxy" into a
// timeout on every picture.
func modalityEgressClient(label string) *http.Client {
	base := egress.For("llm/" + label).HTTPClient()
	wrapped := *base
	wrapped.Timeout = modalityEgressTimeout
	return &wrapped
}
