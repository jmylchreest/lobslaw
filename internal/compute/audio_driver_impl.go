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
	"path/filepath"
	"strings"
)

// Transcribe uploads the recording as multipart form data.
func (d *whisperAudioDriver) Transcribe(ctx context.Context, in AudioRequest) (string, error) {
	client, cfg := d.client, d.cfg
	abs, data, language := in.Filename, in.Data, in.Language
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
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if cfg.Credential != nil {
		if err := cfg.Credential.Apply(ctx, req); err != nil {
			return "", err
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", Transient(fmt.Errorf("read_audio: http: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", &DriverError{
			Class: ClassifyHTTPStatus(resp.StatusCode, string(raw)),
			Err:   fmt.Errorf("read_audio: HTTP %d: %s", resp.StatusCode, TruncateBodyFor(raw, 512)),
		}
	}
	var decoded struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", fmt.Errorf("read_audio: decode: %w", err)
	}
	return decoded.Text, nil
}

// Transcribe sends the recording as an input_audio content part and
// treats the assistant's reply as the transcript.
func (d *chatMultimodalAudioDriver) Transcribe(ctx context.Context, in AudioRequest) (string, error) {
	client, cfg := d.client, d.cfg
	abs, data, language := in.Filename, in.Data, in.Language
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
	req.Header.Set("Content-Type", "application/json")
	if cfg.Credential != nil {
		if err := cfg.Credential.Apply(ctx, req); err != nil {
			return "", err
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", Transient(fmt.Errorf("read_audio: http: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", &DriverError{
			Class: ClassifyHTTPStatus(resp.StatusCode, string(raw)),
			Err:   fmt.Errorf("read_audio: HTTP %d: %s", resp.StatusCode, TruncateBodyFor(raw, 512)),
		}
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
