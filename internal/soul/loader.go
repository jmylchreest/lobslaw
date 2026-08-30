package soul

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/jmylchreest/lobslaw/pkg/types"

	"log/slog"

	"github.com/jmylchreest/lobslaw/internal/promptguard"
)

// Soul is the parsed form of a SOUL.md file: the YAML frontmatter
// lifted into types.SoulConfig plus the freeform markdown body
// that gets injected into the agent's system prompt verbatim.
type Soul struct {
	Config types.SoulConfig
	Body   string // markdown following the frontmatter, empty-whitespace trimmed
	Path   string // absolute path the soul was loaded from
}

// Load reads + parses a SOUL.md at path. Returns ErrNotFound when
// the file is missing so callers can fall back to defaults cleanly.
// Malformed frontmatter / missing required fields return a typed
// error with the file path in the message.
func Load(path string) (*Soul, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("soul: resolve path %q: %w", path, err)
	}
	f, err := os.Open(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("soul: open %q: %w", abs, err)
	}
	defer func() { _ = f.Close() }()

	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("soul: read %q: %w", abs, err)
	}

	// Warn, never block. A SOUL is the agent's identity: refusing to
	// load it on a heuristic would take the assistant down over a
	// false positive, which is a worse outcome than the thing being
	// guarded against.
	//
	// Worth scanning even though it is operator-authored, because it is
	// not only operator-written: the soul_* tools let the agent tune
	// its own identity, so a poisoned memory can drive an edit here.
	// This is the tripwire for that.
	for _, f := range promptguard.Scan(string(raw)) {
		slog.Default().Warn("soul: suspicious content in SOUL file",
			"path", abs, "detector", f.Detector, "detail", f.Detail)
	}

	return Parse(raw, abs)
}

// Parse is the bytes-level entry point used by Load + by tests that
// don't want to touch the filesystem. path is used only for error
// messages — pass "" when parsing a synthetic buffer.
func Parse(raw []byte, path string) (*Soul, error) {
	frontmatter, body, err := splitFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("soul: %s: %w", labelForError(path), err)
	}

	var cfg types.SoulConfig
	if err := yaml.Unmarshal(frontmatter, &cfg); err != nil {
		return nil, fmt.Errorf("soul: %s: parse frontmatter: %w", labelForError(path), err)
	}
	applyDefaults(&cfg)
	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("soul: %s: %w", labelForError(path), err)
	}
	return &Soul{
		Config: cfg,
		Body:   string(bytes.TrimSpace(body)),
		Path:   path,
	}, nil
}

// LoadOrDefault tries Load; on ErrNotFound OR an empty path
// returns a baseline SoulConfig with safe defaults and an empty
// body. Any other error propagates. Callers that want "use SOUL.md
// if present, otherwise run as a neutral assistant" route through
// this rather than branching on os.IsNotExist themselves.
func LoadOrDefault(path string) (*Soul, error) {
	if path == "" {
		return DefaultSoul(), nil
	}
	s, err := Load(path)
	if err == nil {
		return s, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return DefaultSoul(), nil
}

// DefaultSoul returns a minimal Soul for deployments without a
// configured personality. Name + persona_description lean neutral;
// min_trust_tier is intentionally unset so nothing filters the
// provider chain.
func DefaultSoul() *Soul {
	cfg := types.SoulConfig{
		Name:               "assistant",
		Scope:              "default",
		PersonaDescription: "a helpful, concise assistant.",
	}
	applyDefaults(&cfg)
	return &Soul{Config: cfg}
}

// ErrNotFound is returned from Load when the SOUL.md file is
// missing. Callers branch on this to decide "fall back to
// DefaultSoul" vs "propagate as a real error."
var ErrNotFound = errors.New("soul: SOUL.md not found")

// splitFrontmatter finds the leading "---\n...\n---\n" block and
// returns (frontmatter, body). A file with no frontmatter is
// treated as all-body (empty frontmatter). Accepts both CRLF and
// LF line endings — editors on Windows happen.
func splitFrontmatter(raw []byte) ([]byte, []byte, error) {
	normalised := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(normalised, []byte("---\n")) {
		return nil, normalised, nil
	}
	remainder := normalised[len("---\n"):]
	before, after, ok := bytes.Cut(remainder, []byte("\n---\n"))
	if !ok {
		// Handle the case where the file is frontmatter-only with
		// no trailing body (closing --- at EOF, no newline after).
		if bytes.HasSuffix(remainder, []byte("\n---")) {
			return remainder[:len(remainder)-len("\n---")], nil, nil
		}
		return nil, nil, errors.New("frontmatter: no closing '---' marker")
	}
	fm := before
	body := after
	return fm, body, nil
}

// applyDefaults fills in sensible zero-value fallbacks so a sparse
// SOUL.md doesn't require the operator to spell out every field.
func applyDefaults(cfg *types.SoulConfig) {
	if cfg.Scope == "" {
		cfg.Scope = DefaultScope
	}
	if cfg.Language.Default == "" {
		cfg.Language.Default = DefaultLanguage
	}
	if cfg.EmotiveStyle.EmojiUsage == "" {
		cfg.EmotiveStyle.EmojiUsage = DefaultEmojiUsage
	}
	if cfg.Adjustments.FeedbackCoefficient == 0 {
		cfg.Adjustments.FeedbackCoefficient = DefaultFeedbackCoefficient
	}
	if cfg.Adjustments.CooldownPeriod == 0 {
		cfg.Adjustments.CooldownPeriod = DefaultCooldownPeriod
	}
	if cfg.Feedback.Classifier == "" {
		cfg.Feedback.Classifier = DefaultFeedbackClassifier
	}
}

// validate enforces invariants the loader-side can check without
// running a turn. EmotiveStyle dimensions must be 0–10; emoji_usage
// must be one of three labels; feedback.classifier must be a known
// mode. Anything out of range is an operator-config error surfaced
// at boot rather than at first message.
func validate(cfg *types.SoulConfig) error {
	for _, field := range []struct {
		name  string
		value int
	}{
		{DimExcitement, cfg.EmotiveStyle.Excitement},
		{DimFormality, cfg.EmotiveStyle.Formality},
		{DimDirectness, cfg.EmotiveStyle.Directness},
		{DimSarcasm, cfg.EmotiveStyle.Sarcasm},
		{DimHumor, cfg.EmotiveStyle.Humor},
	} {
		if field.value < 0 || field.value > 10 {
			return fmt.Errorf("emotive_style.%s=%d must be 0–10", field.name, field.value)
		}
	}
	switch cfg.EmotiveStyle.EmojiUsage {
	case "minimal", "moderate", "generous":
	default:
		return fmt.Errorf("emotive_style.emoji_usage=%q must be minimal|moderate|generous", cfg.EmotiveStyle.EmojiUsage)
	}
	switch cfg.Feedback.Classifier {
	case "llm", "regex":
	default:
		return fmt.Errorf("feedback.classifier=%q must be llm|regex", cfg.Feedback.Classifier)
	}
	return nil
}

// labelForError picks a readable identifier for an error message —
// the absolute path when we have one, else "<soul>" so the messages
// read naturally when parsing a synthetic buffer.
func labelForError(path string) string {
	if path == "" {
		return "<soul>"
	}
	return path
}
