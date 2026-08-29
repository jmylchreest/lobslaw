package dashscope

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// fakeTTS speaks the server half of the task protocol: accept run-task,
// answer task-started, then on continue-task emit binary audio and
// finish. Recorded messages let a test assert what the driver sent.
type fakeTTS struct {
	srv  *httptest.Server
	sent []map[string]any
	// failWith, when set, is returned as a task-failed error_code
	// instead of running the task.
	failWith string
	// silent skips the audio frames, to exercise finish-with-no-audio.
	silent bool
}

func newFakeTTS(t *testing.T) *fakeTTS {
	t.Helper()
	f := &fakeTTS{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		ctx := r.Context()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				return
			}
			f.sent = append(f.sent, m)
			hdr, _ := m["header"].(map[string]any)
			taskID, _ := hdr["task_id"].(string)

			switch hdr["action"] {
			case "run-task":
				if f.failWith != "" {
					_ = c.Write(ctx, websocket.MessageText, event(taskID, "task-failed", f.failWith,
						"[cosyvoice]Engine error [411]: TTS speak operation failed"))
					return
				}
				_ = c.Write(ctx, websocket.MessageText, event(taskID, "task-started", "", ""))
			case "continue-task":
				if !f.silent {
					_ = c.Write(ctx, websocket.MessageBinary, []byte("ID3audio-part-one"))
					_ = c.Write(ctx, websocket.MessageBinary, []byte("audio-part-two"))
				}
			case "finish-task":
				_ = c.Write(ctx, websocket.MessageText, event(taskID, "task-finished", "", ""))
				return
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTTS) url() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") }

func event(taskID, name, code, msg string) []byte {
	b, _ := json.Marshal(map[string]any{
		"header": map[string]any{
			"task_id": taskID, "event": name,
			"error_code": code, "error_message": msg,
		},
	})
	return b
}

func newSpeak(t *testing.T, f *fakeTTS, voice string) compute.SpeakDriver {
	t.Helper()
	d, err := NewSpeak(SpeakConfig{
		Endpoint:   f.url(),
		Model:      "qwen-audio-3.0-tts-plus",
		Voice:      voice,
		Credential: compute.NewBearerCredential("k"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The audio arrives as BINARY frames interleaved with JSON events, and
// the driver's whole job is to hide that behind a synchronous call.
func TestSpeakAssemblesBinaryFrames(t *testing.T) {
	f := newFakeTTS(t)
	art, err := newSpeak(t, f, "").Speak(context.Background(), compute.SpeakRequest{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(art.Bytes); got != "ID3audio-part-oneaudio-part-two" {
		t.Errorf("frames not assembled in order: %q", got)
	}
	if art.Kind != compute.ArtifactInline || art.MIME != "audio/mpeg" {
		t.Errorf("kind=%s mime=%s", art.Kind, art.MIME)
	}
}

// The text must not be sent until task-started arrives: the protocol
// says the task is not running until the server says so, and anything
// sent before that races the engine's own setup.
func TestSpeakWaitsForTaskStartedBeforeSendingText(t *testing.T) {
	f := newFakeTTS(t)
	if _, err := newSpeak(t, f, "").Speak(context.Background(), compute.SpeakRequest{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 3 {
		t.Fatalf("sent %d messages, want run/continue/finish", len(f.sent))
	}
	var actions []string
	var taskIDs []string
	for _, m := range f.sent {
		h := m["header"].(map[string]any)
		actions = append(actions, h["action"].(string))
		taskIDs = append(taskIDs, h["task_id"].(string))
	}
	if strings.Join(actions, ",") != "run-task,continue-task,finish-task" {
		t.Errorf("actions = %v", actions)
	}
	// One task_id across all three, per the protocol.
	if taskIDs[0] != taskIDs[1] || taskIDs[1] != taskIDs[2] || taskIDs[0] == "" {
		t.Errorf("task ids differ across the task: %v", taskIDs)
	}
}

// Voices are per-model and take no model prefix. An InvalidParameter is
// almost always that, and the vendor's own message names an empty voice
// it never received — so the hint is what makes it actionable.
func TestSpeakExplainsInvalidParameter(t *testing.T) {
	f := newFakeTTS(t)
	f.failWith = "InvalidParameter"
	_, err := newSpeak(t, f, "some-other-models-voice").Speak(
		context.Background(), compute.SpeakRequest{Text: "hello"})
	if err == nil {
		t.Fatal("want an error")
	}
	// The vendor's wording survives...
	if !strings.Contains(err.Error(), "Engine error [411]") {
		t.Errorf("vendor message lost: %v", err)
	}
	// ...and the hint is added to it, naming the voice actually used.
	if !strings.Contains(err.Error(), "some-other-models-voice") {
		t.Errorf("hint does not name the voice: %v", err)
	}
	if compute.ClassifyFailure(err) != compute.FailurePermanent {
		t.Error("a bad voice is permanent; retrying it changes nothing")
	}
}

// A non-InvalidParameter failure must NOT gain the voice hint, or a
// guess about which failure occurred hides the real one.
func TestSpeakDoesNotGuessAtOtherFailures(t *testing.T) {
	f := newFakeTTS(t)
	f.failWith = "Throttling"
	_, err := newSpeak(t, f, "").Speak(context.Background(), compute.SpeakRequest{Text: "hello"})
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "voices are per-model") {
		t.Errorf("voice hint added to an unrelated failure: %v", err)
	}
}

// Finishing with no audio is a failure, not an empty artifact: an empty
// file delivered as speech is worse than an error.
func TestSpeakRejectsSilentTask(t *testing.T) {
	f := newFakeTTS(t)
	f.silent = true
	_, err := newSpeak(t, f, "").Speak(context.Background(), compute.SpeakRequest{Text: "hello"})
	if err == nil || !strings.Contains(err.Error(), "no audio") {
		t.Errorf("want a no-audio error, got %v", err)
	}
}

// An HTTP endpoint here is an operator reusing the one they configured
// for another DashScope modality. Refused at construction with the
// reason, rather than as a dial error naming a scheme.
func TestSpeakRefusesNonWebSocketEndpoint(t *testing.T) {
	_, err := NewSpeak(SpeakConfig{
		Endpoint:   "https://dashscope-intl.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation",
		Model:      "qwen-audio-3.0-tts-plus",
		Credential: compute.NewBearerCredential("k"),
	})
	if err == nil || !strings.Contains(err.Error(), "WebSocket") {
		t.Errorf("want a scheme error explaining why, got %v", err)
	}
}

func TestSpeakRequiresCredentialAndModel(t *testing.T) {
	if _, err := NewSpeak(SpeakConfig{Model: "m"}); err == nil {
		t.Error("want an error with no credential")
	}
	if _, err := NewSpeak(SpeakConfig{Credential: compute.NewBearerCredential("k")}); err == nil {
		t.Error("want an error with no model")
	}
}

func TestSpeakMIMEFollowsFormat(t *testing.T) {
	for _, tc := range []struct{ format, want string }{
		{"mp3", "audio/mpeg"}, {"wav", "audio/wav"},
		{"opus", "audio/opus"}, {"pcm", "audio/L16"}, {"", "audio/mpeg"},
	} {
		if got := speakMIME(tc.format); got != tc.want {
			t.Errorf("speakMIME(%q) = %q, want %q", tc.format, got, tc.want)
		}
	}
}
