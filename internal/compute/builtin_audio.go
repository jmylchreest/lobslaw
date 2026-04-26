package compute

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// AudioFormat picks the wire protocol for read_audio.
type AudioFormat string

const (
	// AudioFormatWhisper is the OpenAI /v1/audio/transcriptions
	// multipart POST. Default. Covers OpenAI, MiniMax STT, and any
	// self-hosted server that exposes the Whisper API surface
	// (faster-whisper-server, parakeet wrapped behind a thin
	// FastAPI shim, etc).
	AudioFormatWhisper AudioFormat = "openai"

	// AudioFormatChatMultimodal is OpenRouter's audio-on-chat shape:
	// /v1/chat/completions with content parts that include
	// {type:"input_audio", input_audio:{data:"<b64>", format:"wav"}}.
	// Returns a normal chat completion; we treat the assistant's
	// content as the transcript.
	AudioFormatChatMultimodal AudioFormat = "openrouter"
)

// AudioConfig wires the read_audio (STT) builtin. Format selects
// between Whisper-style multipart (default) and OpenRouter's
// chat-completions-multimodal shape. For self-hosted Parakeet /
// faster-whisper sidecars: leave Format empty (whisper) and point
// Endpoint at the local URL.
type AudioConfig struct {
	Endpoint    string
	Model       string
	APIKey      string
	Format      AudioFormat
	AllowedRoot string
	HTTPClient  *http.Client
}

// RegisterAudioBuiltin installs read_audio. Required-fields check
// matches the vision builtin so misconfigurations surface loudly.
func RegisterAudioBuiltin(b *Builtins, cfg AudioConfig) error {
	if cfg.Endpoint == "" || cfg.APIKey == "" {
		return errors.New("read_audio: Endpoint and APIKey both required")
	}
	if cfg.Model == "" {
		return errors.New("read_audio: Model required (e.g. \"whisper-1\", \"speech-01\", \"google/gemini-2.0-flash-001\")")
	}
	if cfg.Format == "" {
		cfg.Format = AudioFormatWhisper
	}
	switch cfg.Format {
	case AudioFormatWhisper, AudioFormatChatMultimodal:
	default:
		return fmt.Errorf("read_audio: unknown format %q (want openai|openrouter)", cfg.Format)
	}
	if cfg.AllowedRoot == "" {
		cfg.AllowedRoot = "/workspace/incoming"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	return b.Register("read_audio", newReadAudioHandler(cfg, client))
}

// AudioToolDef is the ToolDef registered alongside the builtin.
func AudioToolDef() *types.ToolDef {
	return &types.ToolDef{
		Name:        "read_audio",
		Path:        BuiltinScheme + "read_audio",
		Description: "Transcribe an audio file (voice note, recording) at a local path to text. Channel layer downloads inbound voice/audio attachments to /workspace/incoming/<turn>/<file> and surfaces the path via [user attached: voice ... path=...]. When the user sends a voice note or audio file, ALWAYS call read_audio with that path before answering — the main model can't ingest audio directly. Optional language hint (BCP-47, e.g. \"en\", \"de\") improves accuracy on accented or non-English speech. Returns the transcript as content.",
		ParametersSchema: []byte(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Absolute path to the audio file (typically /workspace/incoming/<turn>/<file>)."},
				"language": {"type": "string", "description": "Optional BCP-47 language hint (e.g. \"en\", \"de\"). Empty → autodetect."}
			},
			"required": ["path"],
			"additionalProperties": false
		}`),
		RiskTier: types.RiskCommunicating,
	}
}

func newReadAudioHandler(cfg AudioConfig, client *http.Client) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		path := strings.TrimSpace(args["path"])
		if path == "" {
			return nil, 2, errors.New("read_audio: path is required")
		}
		language := strings.TrimSpace(args["language"])

		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, 2, fmt.Errorf("read_audio: resolve path: %w", err)
		}
		if cfg.AllowedRoot != "" {
			rootAbs, err := filepath.Abs(cfg.AllowedRoot)
			if err != nil {
				return nil, 1, fmt.Errorf("read_audio: resolve allowed root: %w", err)
			}
			if !strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) && abs != rootAbs {
				return nil, 2, fmt.Errorf("read_audio: path %q outside allowed root %q", abs, rootAbs)
			}
		}

		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, 2, fmt.Errorf("read_audio: read file: %w", err)
		}

		var transcript string
		switch cfg.Format {
		case AudioFormatWhisper:
			transcript, err = audioWhisperTranscribe(ctx, client, cfg, abs, data, language)
		case AudioFormatChatMultimodal:
			transcript, err = audioChatMultimodalTranscribe(ctx, client, cfg, abs, data, language)
		}
		if err != nil {
			return nil, 1, err
		}
		if transcript == "" {
			return nil, 1, errors.New("read_audio: provider returned empty transcript")
		}

		out, _ := json.Marshal(map[string]any{
			"path":    abs,
			"model":   cfg.Model,
			"format":  string(cfg.Format),
			"content": transcript,
		})
		return out, 0, nil
	}
}

// audioWhisperTranscribe POSTs multipart/form-data to a
// Whisper-API-compatible endpoint. Voice notes from Telegram are
// OPUS-in-OGG; the part Content-Type matters because OpenAI's
// ffmpeg-side detection trusts it.
func audioWhisperTranscribe(ctx context.Context, client *http.Client, cfg AudioConfig, abs string, data []byte, language string) (string, error) {
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	filePart, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": []string{
			fmt.Sprintf(`form-data; name="file"; filename=%q`, filepath.Base(abs)),
		},
		"Content-Type": []string{sniffAudioMime(abs)},
	})
	if err != nil {
		return "", fmt.Errorf("read_audio: build form: %w", err)
	}
	if _, err := filePart.Write(data); err != nil {
		return "", fmt.Errorf("read_audio: write form: %w", err)
	}
	if err := mw.WriteField("model", cfg.Model); err != nil {
		return "", err
	}
	if err := mw.WriteField("response_format", "json"); err != nil {
		return "", err
	}
	if language != "" {
		if err := mw.WriteField("language", language); err != nil {
			return "", err
		}
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("read_audio: close form: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("read_audio: http: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("read_audio: HTTP %d: %s", resp.StatusCode, truncateBodyFor(raw, 512))
	}
	var decoded struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", fmt.Errorf("read_audio: decode: %w", err)
	}
	return decoded.Text, nil
}

// audioChatMultimodalTranscribe POSTs JSON to OpenRouter's
// /v1/chat/completions with an input_audio content part. Returns
// the assistant's content as the transcript. Per OpenRouter's
// docs, the format field within input_audio expects "wav" / "mp3"
// / "ogg" — not the MIME type.
func audioChatMultimodalTranscribe(ctx context.Context, client *http.Client, cfg AudioConfig, abs string, data []byte, language string) (string, error) {
	prompt := "Transcribe this audio verbatim."
	if language != "" {
		prompt = "Transcribe this audio verbatim. The speaker is using language: " + language + "."
	}
	b64 := encodeBase64(data)
	format := audioContainerExt(abs)
	reqBody, _ := json.Marshal(audioChatMultimodalRequest{
		Model:     cfg.Model,
		MaxTokens: 1024,
		Messages: []audioChatMessage{{
			Role: "user",
			Content: []audioChatPart{
				{Type: "text", Text: prompt},
				{Type: "input_audio", InputAudio: &audioInputAudio{Data: b64, Format: format}},
			},
		}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("read_audio: http: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("read_audio: HTTP %d: %s", resp.StatusCode, truncateBodyFor(raw, 512))
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", fmt.Errorf("read_audio: decode: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return "", errors.New("no choices")
	}
	return decoded.Choices[0].Message.Content, nil
}

// audioContainerExt returns OpenRouter's expected `format` token
// for the input_audio content part — it's the bare extension, not
// a MIME type. .ogg (Telegram voice) → "ogg".
func audioContainerExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ogg", ".opus":
		return "ogg"
	case ".mp3":
		return "mp3"
	case ".wav":
		return "wav"
	case ".m4a":
		return "m4a"
	case ".flac":
		return "flac"
	case ".webm":
		return "webm"
	}
	return "wav"
}

func encodeBase64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

type audioChatMultimodalRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens,omitempty"`
	Messages  []audioChatMessage `json:"messages"`
}
type audioChatMessage struct {
	Role    string          `json:"role"`
	Content []audioChatPart `json:"content"`
}
type audioChatPart struct {
	Type       string           `json:"type"`
	Text       string           `json:"text,omitempty"`
	InputAudio *audioInputAudio `json:"input_audio,omitempty"`
}
type audioInputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

// sniffAudioMime maps common voice/audio extensions to MIME types.
// Telegram voice notes are OPUS-in-OGG (.ogg → audio/ogg).
func sniffAudioMime(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ogg", ".opus":
		return "audio/ogg"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".m4a":
		return "audio/mp4"
	case ".flac":
		return "audio/flac"
	case ".webm":
		return "audio/webm"
	}
	return "application/octet-stream"
}
