package soul

import (
	"context"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// botTuneStore is a TuneStore that also serves per-bot overlays. The
// chief's overlay is the one the plain Get path returns, matching how
// the raft store keys them.
type botTuneStore struct {
	*MemoryTuneStore
	perBot map[string]*TuneState
	asked  []string
}

func (s *botTuneStore) GetFor(ctx context.Context, botID string) (*TuneState, error) {
	s.asked = append(s.asked, botID)
	if t, ok := s.perBot[botID]; ok {
		return t, nil
	}
	return s.Get(ctx)
}

func testBaseline() *Soul {
	return &Soul{
		Config: types.SoulConfig{
			Name: "Baseline",
			EmotiveStyle: types.EmotiveStyle{
				Sarcasm: 5, Humor: 5, Formality: 5, Directness: 5, Excitement: 5,
			},
		},
		Body: "Operator house style.",
	}
}

func overlayPtr[T any](v T) *T { return &v }

func newBotAdjuster(t *testing.T, store TuneStore) *Adjuster {
	t.Helper()
	a, err := NewAdjuster(AdjusterConfig{Soul: testBaseline(), Store: store})
	if err != nil {
		t.Fatalf("NewAdjuster: %v", err)
	}
	return a
}

// Each bot reads its own overlay. The whole point of per-bot souls: a
// marketing bot tuned chatty must not make the devops bot chatty.
func TestEachBotReadsItsOwnOverlay(t *testing.T) {
	t.Parallel()
	store := &botTuneStore{
		MemoryTuneStore: &MemoryTuneStore{},
		perBot: map[string]*TuneState{
			"marketing": {Humor: overlayPtr(9)},
			"devops":    {Humor: overlayPtr(1)},
		},
	}
	a := newBotAdjuster(t, store)

	marketing, err := a.SnapshotFor(context.Background(), "marketing")
	if err != nil {
		t.Fatalf("SnapshotFor marketing: %v", err)
	}
	devops, err := a.SnapshotFor(context.Background(), "devops")
	if err != nil {
		t.Fatalf("SnapshotFor devops: %v", err)
	}

	if marketing.Config.EmotiveStyle.Humor == devops.Config.EmotiveStyle.Humor {
		t.Fatalf("both bots resolved to humor=%d; overlays are not per-bot",
			marketing.Config.EmotiveStyle.Humor)
	}
	if marketing.Config.EmotiveStyle.Humor <= devops.Config.EmotiveStyle.Humor {
		t.Errorf("marketing humor %d should exceed devops %d",
			marketing.Config.EmotiveStyle.Humor, devops.Config.EmotiveStyle.Humor)
	}
}

// The operator's SOUL.md is the house style every bot shares, so a bot
// with no overlay of its own serves the baseline rather than nothing.
func TestBotWithNoOverlayServesTheOperatorBaseline(t *testing.T) {
	t.Parallel()
	store := &botTuneStore{MemoryTuneStore: &MemoryTuneStore{}, perBot: map[string]*TuneState{}}
	a := newBotAdjuster(t, store)

	got, err := a.SnapshotFor(context.Background(), "fresh")
	if err != nil {
		t.Fatalf("SnapshotFor: %v", err)
	}
	if got.Config.Name != "Baseline" {
		t.Errorf("name = %q, want the operator baseline", got.Config.Name)
	}
	if got.Body != "Operator house style." {
		t.Errorf("body = %q, want the operator's", got.Body)
	}
}

// "Be less sarcastic with me", said to the chief in Telegram, is about
// the chief. Having it silently re-tune the devops bot would be
// action-at-a-distance nobody would connect to the sentence that
// caused it.
func TestTuningTheChiefDoesNotRetuneOtherBots(t *testing.T) {
	t.Parallel()
	chiefOverlay := &MemoryTuneStore{}
	if err := chiefOverlay.Put(context.Background(), &TuneState{Sarcasm: overlayPtr(0)}); err != nil {
		t.Fatalf("seed chief overlay: %v", err)
	}
	store := &botTuneStore{
		MemoryTuneStore: chiefOverlay,
		perBot:          map[string]*TuneState{"devops": nil},
	}
	a := newBotAdjuster(t, store)

	devops, err := a.SnapshotFor(context.Background(), "devops")
	if err != nil {
		t.Fatalf("SnapshotFor: %v", err)
	}
	if got := devops.Config.EmotiveStyle.Sarcasm; got != 5 {
		t.Errorf("devops sarcasm = %d, want the baseline 5 — the chief's tune leaked", got)
	}
}

// A store predating bots keeps working: every turn gets the overlay it
// always got, so an upgrade changes nothing for a deployment that
// never creates a bot.
func TestStoreWithoutPerBotSupportFallsBackToTheChiefOverlay(t *testing.T) {
	t.Parallel()
	plain := &MemoryTuneStore{}
	if err := plain.Put(context.Background(), &TuneState{Humor: overlayPtr(8)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newBotAdjuster(t, plain)

	got, err := a.SnapshotFor(context.Background(), "anything")
	if err != nil {
		t.Fatalf("SnapshotFor: %v", err)
	}
	if got.Config.EmotiveStyle.Humor != 8 {
		t.Errorf("humor = %d, want the existing overlay's 8", got.Config.EmotiveStyle.Humor)
	}
}

// An empty bot id is the node default and must not take the per-bot
// path — a channel that has not been taught about bots passes it.
func TestEmptyBotIDTakesTheDefaultPath(t *testing.T) {
	t.Parallel()
	store := &botTuneStore{MemoryTuneStore: &MemoryTuneStore{}, perBot: map[string]*TuneState{}}
	a := newBotAdjuster(t, store)

	if _, err := a.SnapshotFor(context.Background(), ""); err != nil {
		t.Fatalf("SnapshotFor: %v", err)
	}
	if len(store.asked) != 0 {
		t.Errorf("the per-bot path was taken for an empty bot id: %v", store.asked)
	}
}

// The drift clamp must apply identically however the overlay arrived.
// A bot whose cap was enforced differently from the chief's would be a
// difference nobody finds by reading either one.
func TestBotOverlayIsClampedLikeTheChiefs(t *testing.T) {
	t.Parallel()
	store := &botTuneStore{
		MemoryTuneStore: &MemoryTuneStore{},
		perBot:          map[string]*TuneState{"extreme": {Sarcasm: overlayPtr(10)}},
	}
	a := newBotAdjuster(t, store)

	got, err := a.SnapshotFor(context.Background(), "extreme")
	if err != nil {
		t.Fatalf("SnapshotFor: %v", err)
	}
	if want := 5 + MaxDriftFromBaseline; got.Config.EmotiveStyle.Sarcasm != want {
		t.Errorf("sarcasm = %d, want it clamped to %d", got.Config.EmotiveStyle.Sarcasm, want)
	}
}
