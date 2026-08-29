package dashscope

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/ids"
)

// Qwen TTS, which is a WebSocket task protocol rather than a request.
//
// Every other speak driver POSTs text and reads audio out of the
// response. DashScope instead opens a socket, runs a three-message task
// on it — run-task, continue-task, finish-task, all sharing one task_id
// — and streams the audio back as BINARY frames interleaved with JSON
// events. That is the whole reason this is not another
// OpenAISpeakDriver endpoint.
//
// SpeakDriver is synchronous and returns an Artifact, so the streaming
// is an implementation detail: this accumulates the frames and hands
// back the assembled bytes. Nothing above it needs to know.

// DefaultSpeakEndpoint is the DashScope inference socket.
const DefaultSpeakEndpoint = "wss://dashscope-intl.aliyuncs.com/api-ws/v1/inference"

// defaultSpeakVoice is a system voice for qwen-audio-3.0-tts-plus.
//
// Named rather than left empty because the engine refuses a task with
// no voice, and the refusal is unhelpful: `[cosyvoice:]Engine error
// [411]` with the empty brackets being the voice it did not get.
//
// VOICES ARE PER-MODEL and carry no model prefix. A voice from the
// flash model's list is an InvalidParameter against plus, and
// "qwen-audio-3.0-tts-plus-longanlingxin" — the pattern the base-voice
// catalogue is named by — is refused where bare "longanlingxin" works.
// Both verified against the live API.
const defaultSpeakVoice = "longanlingxin"

// defaultSpeakSampleRate follows the vendor's own example.
const defaultSpeakSampleRate = 22050

// speakTaskTimeout bounds one synthesis. Generous next to the tree's
// other audio work because this is a streamed task: the deadline has to
// cover the whole run-to-finish exchange, not one round trip.
const speakTaskTimeout = 60 * time.Second

// SpeakConfig wires the Qwen TTS driver.
type SpeakConfig struct {
	Endpoint   string
	Model      string
	Voice      string
	Format     string
	Credential compute.Credential
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// NewSpeak builds the driver.
func NewSpeak(cfg SpeakConfig) (*SpeakDriver, error) {
	if cfg.Credential == nil {
		return nil, fmt.Errorf("dashscope speak: credential required")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("dashscope speak: model required")
	}
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = DefaultSpeakEndpoint
	}
	// An operator who configured the HTTP endpoint for another
	// DashScope modality and reused it here would otherwise get a dial
	// error naming a scheme they did not think about.
	if !strings.HasPrefix(endpoint, "ws://") && !strings.HasPrefix(endpoint, "wss://") {
		return nil, fmt.Errorf("dashscope speak: endpoint %q must be ws:// or wss:// — "+
			"Qwen TTS is a WebSocket task, not an HTTP request", endpoint)
	}
	voice := strings.TrimSpace(cfg.Voice)
	if voice == "" {
		voice = defaultSpeakVoice
	}
	format := strings.TrimSpace(cfg.Format)
	if format == "" {
		format = compute.DefaultSpeakFormat
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &SpeakDriver{
		endpoint: endpoint,
		model:    cfg.Model,
		voice:    voice,
		format:   format,
		cred:     cfg.Credential,
		client:   cfg.HTTPClient,
		log:      log,
	}, nil
}

// SpeakDriver renders text through Qwen TTS.
type SpeakDriver struct {
	endpoint string
	model    string
	voice    string
	format   string
	cred     compute.Credential
	client   *http.Client
	log      *slog.Logger
}

// taskMessage is every client message. The three actions differ only in
// which payload fields they carry, so one shape covers all of them and
// the omitempty tags keep the wire clean.
type taskMessage struct {
	Header struct {
		Action    string `json:"action"`
		TaskID    string `json:"task_id"`
		Streaming string `json:"streaming"`
	} `json:"header"`
	Payload struct {
		TaskGroup  string         `json:"task_group,omitempty"`
		Task       string         `json:"task,omitempty"`
		Function   string         `json:"function,omitempty"`
		Model      string         `json:"model,omitempty"`
		Parameters map[string]any `json:"parameters,omitempty"`
		Input      map[string]any `json:"input"`
	} `json:"payload"`
}

// taskEvent is the server's side. error_code and error_message are set
// only on task-failed.
type taskEvent struct {
	Header struct {
		TaskID       string `json:"task_id"`
		Event        string `json:"event"`
		ErrorCode    string `json:"error_code"`
		ErrorMessage string `json:"error_message"`
	} `json:"header"`
}

// Speak runs one synthesis task and returns the assembled audio.
func (d *SpeakDriver) Speak(ctx context.Context, req compute.SpeakRequest) (*compute.Artifact, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, compute.Permanent(fmt.Errorf("dashscope speak: text required"))
	}
	model := req.Model
	if model == "" {
		model = d.model
	}
	voice := strings.TrimSpace(req.Voice)
	if voice == "" {
		voice = d.voice
	}
	format := strings.TrimSpace(req.Format)
	if format == "" {
		format = d.format
	}

	ctx, cancel := context.WithTimeout(ctx, speakTaskTimeout)
	defer cancel()

	// The credential is applied to the HANDSHAKE, not to a frame: the
	// upgrade is an ordinary HTTP request and this is the only point at
	// which a header can be set.
	header := http.Header{}
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://dashscope.invalid", nil)
	if err != nil {
		return nil, compute.Permanent(fmt.Errorf("dashscope speak: build handshake: %w", err))
	}
	if err := d.cred.Apply(ctx, probe); err != nil {
		return nil, compute.Permanent(fmt.Errorf("dashscope speak: apply credential: %w", err))
	}
	for k, v := range probe.Header {
		header[k] = v
	}

	conn, resp, err := websocket.Dial(ctx, d.endpoint, &websocket.DialOptions{
		HTTPHeader: header,
		HTTPClient: d.client,
	})
	if err != nil {
		// A rejected upgrade carries its status, and 401 against 503 is
		// the difference between a wrong key and an outage.
		if resp != nil {
			return nil, &compute.DriverError{
				Class: compute.ClassifyHTTPStatus(resp.StatusCode, ""),
				Err:   fmt.Errorf("dashscope speak: handshake HTTP %d: %w", resp.StatusCode, err),
			}
		}
		return nil, compute.Transient(fmt.Errorf("dashscope speak: dial: %w", err))
	}
	// CloseNow, not Close: the graceful close writes a frame and waits
	// for the peer's reply, which is the wrong tool on a path that may
	// be abandoning a failed task.
	defer func() { _ = conn.CloseNow() }()

	taskID := ids.New()
	if err := d.send(ctx, conn, d.runTask(taskID, model, voice, format)); err != nil {
		return nil, err
	}
	return d.collect(ctx, conn, taskID, text, format)
}

func (d *SpeakDriver) runTask(taskID, model, voice, format string) taskMessage {
	var m taskMessage
	m.Header.Action = "run-task"
	m.Header.TaskID = taskID
	m.Header.Streaming = "duplex"
	m.Payload.TaskGroup = "audio"
	m.Payload.Task = "tts"
	m.Payload.Function = "SpeechSynthesizer"
	m.Payload.Model = model
	m.Payload.Parameters = map[string]any{
		"text_type":   "PlainText",
		"voice":       voice,
		"format":      format,
		"sample_rate": defaultSpeakSampleRate,
	}
	m.Payload.Input = map[string]any{}
	return m
}

func (d *SpeakDriver) send(ctx context.Context, conn *websocket.Conn, m taskMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return compute.Permanent(fmt.Errorf("dashscope speak: marshal %s: %w", m.Header.Action, err))
	}
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		return compute.Transient(fmt.Errorf("dashscope speak: write %s: %w", m.Header.Action, err))
	}
	return nil
}

// collect drives the task to completion and assembles the audio.
//
// The text is not sent until task-started arrives. Sending it with the
// run-task, or immediately after, races the engine's own setup — the
// protocol says the task is not running until it says so, and the
// server is entitled to reject anything before that.
func (d *SpeakDriver) collect(
	ctx context.Context, conn *websocket.Conn, taskID, text, format string,
) (*compute.Artifact, error) {
	var audio bytes.Buffer
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, compute.Permanent(fmt.Errorf("dashscope speak: %w", ctx.Err()))
			}
			return nil, compute.Transient(fmt.Errorf("dashscope speak: read: %w", err))
		}
		if typ == websocket.MessageBinary {
			audio.Write(data)
			continue
		}

		var ev taskEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, compute.Permanent(fmt.Errorf("dashscope speak: malformed event: %w", err))
		}
		switch ev.Header.Event {
		case "task-started":
			var cont taskMessage
			cont.Header.Action = "continue-task"
			cont.Header.TaskID = taskID
			cont.Header.Streaming = "duplex"
			cont.Payload.Input = map[string]any{"text": text}
			if err := d.send(ctx, conn, cont); err != nil {
				return nil, err
			}
			var fin taskMessage
			fin.Header.Action = "finish-task"
			fin.Header.TaskID = taskID
			fin.Header.Streaming = "duplex"
			fin.Payload.Input = map[string]any{}
			if err := d.send(ctx, conn, fin); err != nil {
				return nil, err
			}

		case "task-finished":
			if audio.Len() == 0 {
				return nil, compute.Transient(fmt.Errorf(
					"dashscope speak: task finished with no audio"))
			}
			return &compute.Artifact{
				Kind:  compute.ArtifactInline,
				Bytes: audio.Bytes(),
				MIME:  speakMIME(format),
			}, nil

		case "task-failed":
			return nil, d.taskFailure(ev)

		case "result-generated":
			// Progress. The audio itself arrives on the binary channel,
			// so there is nothing to take from this.

		default:
			d.log.Debug("dashscope speak: unrecognised event", "event", ev.Header.Event)
		}
	}
}

// taskFailure turns a task-failed event into a classified error, and
// adds the fix where the vendor's own wording does not carry one.
//
// An InvalidParameter here is almost always the voice: voices are
// per-model, and one from another model's list produces exactly this
// with the engine naming an empty voice it never received. The hint is
// ADDED to the vendor's message, never substituted, so guessing wrong
// about which InvalidParameter this is cannot hide the real one.
func (d *SpeakDriver) taskFailure(ev taskEvent) error {
	msg := strings.TrimSpace(ev.Header.ErrorMessage)
	if msg == "" {
		msg = "task failed with no message"
	}
	err := fmt.Errorf("dashscope speak: %s: %s", ev.Header.ErrorCode, msg)
	if ev.Header.ErrorCode == "InvalidParameter" {
		return compute.Permanent(fmt.Errorf(
			"%w (voices are per-model and take no model prefix — check that %q is in this model's voice list)",
			err, d.voice))
	}
	return compute.Permanent(err)
}

// speakMIME maps the requested container onto a media type. Deliberately
// a small table for the formats the vendor accepts rather than
// mime.TypeByExtension, which is platform-dependent.
func speakMIME(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "wav":
		return "audio/wav"
	case "pcm":
		return "audio/L16"
	case "opus":
		return "audio/opus"
	default:
		return "audio/mpeg"
	}
}
