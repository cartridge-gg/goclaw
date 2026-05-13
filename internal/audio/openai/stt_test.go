package openai_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/audio"
	"github.com/nextlevelbuilder/goclaw/internal/audio/openai"
)

func TestSTTProvider_TranscribeMultipart(t *testing.T) {
	var gotAuth string
	var gotModel string
	var gotLanguage string
	var gotResponseFormat string
	var gotFile string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/audio/transcriptions" {
			t.Errorf("path = %s, want /audio/transcriptions", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")

		mr, err := r.MultipartReader()
		if err != nil {
			t.Fatalf("multipart reader: %v", err)
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("next part: %v", err)
			}
			data, err := io.ReadAll(part)
			if err != nil {
				t.Fatalf("read part: %v", err)
			}
			switch part.FormName() {
			case "model":
				gotModel = string(data)
			case "language":
				gotLanguage = string(data)
			case "response_format":
				gotResponseFormat = string(data)
			case "file":
				gotFile = part.FileName() + ":" + string(data)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"text":     "hello there",
			"language": "en",
			"duration": 1.25,
		})
	}))
	t.Cleanup(srv.Close)

	p := openai.NewSTTProvider(openai.STTConfig{APIKey: "sk-test", APIBase: srv.URL})
	res, err := p.Transcribe(t.Context(), audio.STTInput{
		Bytes:    []byte("oggdata"),
		MimeType: "audio/ogg",
		Filename: "voice.ogg",
	}, audio.STTOptions{Language: "en"})
	if err != nil {
		t.Fatalf("Transcribe failed: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotModel != "gpt-4o-transcribe" {
		t.Fatalf("model = %q", gotModel)
	}
	if gotLanguage != "en" {
		t.Fatalf("language = %q", gotLanguage)
	}
	if gotResponseFormat != "json" {
		t.Fatalf("response_format = %q", gotResponseFormat)
	}
	if !strings.HasPrefix(gotFile, "voice.ogg:oggdata") {
		t.Fatalf("file = %q", gotFile)
	}
	if res.Text != "hello there" || res.Language != "en" || res.Duration != 1.25 || res.Provider != "openai" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestSTTProvider_APIErrorIncludesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"quota exceeded"}}`, http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	p := openai.NewSTTProvider(openai.STTConfig{APIKey: "sk-test", APIBase: srv.URL})
	_, err := p.Transcribe(t.Context(), audio.STTInput{
		Bytes:    []byte("audio"),
		MimeType: "audio/ogg",
	}, audio.STTOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "openai stt: API error 429") {
		t.Fatalf("error = %q", err)
	}
}
