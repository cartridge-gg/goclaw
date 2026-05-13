package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/audio"
)

const (
	sttDefaultModel   = "gpt-4o-transcribe"
	sttDefaultTimeout = 60 * time.Second
)

// STTConfig bundles credentials and defaults for OpenAI transcription.
type STTConfig struct {
	APIKey    string
	APIBase   string // default "https://api.openai.com/v1"
	Model     string // default "gpt-4o-transcribe"
	TimeoutMs int    // default 60000
}

// STTProvider transcribes audio via OpenAI's audio transcription API.
type STTProvider struct {
	apiKey    string
	apiBase   string
	model     string
	timeoutMs int
}

// NewSTTProvider constructs an OpenAI STT provider with defaults applied.
func NewSTTProvider(cfg STTConfig) *STTProvider {
	if cfg.APIBase == "" {
		cfg.APIBase = "https://api.openai.com/v1"
	}
	if cfg.Model == "" {
		cfg.Model = sttDefaultModel
	}
	if cfg.TimeoutMs <= 0 {
		cfg.TimeoutMs = int(sttDefaultTimeout.Milliseconds())
	}
	return &STTProvider{
		apiKey:    cfg.APIKey,
		apiBase:   strings.TrimRight(cfg.APIBase, "/"),
		model:     cfg.Model,
		timeoutMs: cfg.TimeoutMs,
	}
}

// Name returns the stable provider identifier used by the Manager.
func (p *STTProvider) Name() string { return "openai" }

// Transcribe converts audio to text via POST /audio/transcriptions.
func (p *STTProvider) Transcribe(ctx context.Context, in audio.STTInput, opts audio.STTOptions) (*audio.TranscriptResult, error) {
	filePath, cleanup, err := resolveSTTFilePath(in)
	if err != nil {
		return nil, fmt.Errorf("openai stt: resolve input: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	model := opts.ModelID
	if model == "" {
		model = p.model
	}
	if err := mw.WriteField("model", model); err != nil {
		return nil, fmt.Errorf("openai stt: write model field: %w", err)
	}
	if opts.Language != "" {
		if err := mw.WriteField("language", opts.Language); err != nil {
			return nil, fmt.Errorf("openai stt: write language field: %w", err)
		}
	}
	if err := mw.WriteField("response_format", "json"); err != nil {
		return nil, fmt.Errorf("openai stt: write response_format field: %w", err)
	}

	filename := in.Filename
	if filename == "" {
		filename = filepath.Base(filePath)
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return nil, fmt.Errorf("openai stt: create form file: %w", err)
	}
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("openai stt: open file: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(fw, f); err != nil {
		return nil, fmt.Errorf("openai stt: write file bytes: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("openai stt: close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+"/audio/transcriptions", &buf)
	if err != nil {
		return nil, fmt.Errorf("openai stt: create request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	timeout := time.Duration(p.timeoutMs) * time.Millisecond
	if opts.TimeoutMs > 0 {
		timeout = time.Duration(opts.TimeoutMs) * time.Millisecond
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai stt: http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("openai stt: API error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Text     string  `json:"text"`
		Language string  `json:"language"`
		Duration float64 `json:"duration"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("openai stt: parse response: %w", err)
	}

	return &audio.TranscriptResult{
		Text:     result.Text,
		Language: result.Language,
		Duration: result.Duration,
		Provider: "openai",
	}, nil
}

func resolveSTTFilePath(in audio.STTInput) (path string, cleanup func(), err error) {
	if in.FilePath != "" {
		return in.FilePath, nil, nil
	}
	if len(in.Bytes) == 0 {
		return "", nil, fmt.Errorf("neither FilePath nor Bytes provided")
	}
	ext := extFromSTTMime(in.MimeType)
	f, err := os.CreateTemp("", "openai-stt-*"+ext)
	if err != nil {
		return "", nil, fmt.Errorf("create temp file: %w", err)
	}
	if err := os.Chmod(f.Name(), 0600); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := f.Write(in.Bytes); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("close temp file: %w", err)
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

func extFromSTTMime(mime string) string {
	m := strings.ToLower(mime)
	switch {
	case strings.Contains(m, "wav"):
		return ".wav"
	case strings.Contains(m, "mp3"), strings.Contains(m, "mpeg"):
		return ".mp3"
	case strings.Contains(m, "m4a"), strings.Contains(m, "mp4"):
		return ".m4a"
	case strings.Contains(m, "ogg"), strings.Contains(m, "opus"):
		return ".ogg"
	case strings.Contains(m, "flac"):
		return ".flac"
	case strings.Contains(m, "webm"):
		return ".webm"
	default:
		return ".mp3"
	}
}
