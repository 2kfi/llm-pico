package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The upstream model is resolved by routing; the client-facing model name in
// the request body must never be sent to a provider that differs.
func TestGeminiEmbeddingsUseRoutedModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := string(readCheap(r))
		if strings.Contains(body, `"model":"models/gemini-embed"`) {
			t.Errorf("caller-facing name leaked upstream: %s", body)
		}
		if !strings.Contains(r.URL.Path, "gemini-embedding-2") {
			t.Errorf("path = %q, want the routed model in the path", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"embeddings":[{"values":[1,2,3]}]}`))
	}))
	defer srv.Close()

	a := NewGeminiAdapter(srv.URL+"/v1beta", nil, newTestClient())
	resp, err := a.Embeddings(context.Background(), Request{
		Body:   []byte(`{"model":"gemini-embed","input":"hello"}`),
		Model:  "gemini/gemini-embedding-2",
		APIKey: "k",
	})
	if err != nil {
		t.Fatalf("Embeddings: %v", err)
	}
	if !strings.Contains(string(resp.Body), `"index":0`) {
		t.Errorf("body = %s", resp.Body)
	}
}

// The multipart form must carry the routed model, not the caller's name.
func TestOpenAITranscribeUsesRoutedModel(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(1 << 20)
		gotModel = r.FormValue("model")
		_, _ = w.Write([]byte(`{"text":"hi"}`))
	}))
	defer srv.Close()

	a := NewOpenAIAdapter(srv.URL, AuthHeaderNames{}, newTestClient())
	if _, err := a.Transcribe(context.Background(), Request{
		Body:   []byte(`{"model":"whisper-1","response_format":"json"}`),
		Model:  "openai/whisper-large-v3",
		APIKey: "k",
	}, AudioFile{Filename: "a.wav", Data: []byte("xx")}); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if gotModel != "whisper-large-v3" {
		t.Errorf("upstream model = %q, want the routed model", gotModel)
	}
}

func readCheap(r *http.Request) []byte {
	b := make([]byte, 4096)
	n, _ := r.Body.Read(b)
	return b[:n]
}
