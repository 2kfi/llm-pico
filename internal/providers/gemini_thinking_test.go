package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestToGeminiMapsReasoningEffort(t *testing.T) {
	cases := map[string]int{
		`"reasoning_effort": "none"`:   0,
		`"reasoning_effort": "low"`:    1024,
		`"reasoning_effort": "medium"`: 8192,
		`"reasoning_effort": "high"`:   24576,
		`"thinking_budget": 512`:       512,
		// 0 is an explicit request to disable thinking, not an absent field,
		// and -1 lets the model decide; both must survive the wire format.
		`"thinking_budget": 0`:  0,
		`"thinking_budget": -1`: -1,
		// the explicit budget beats the preset, including a disabling one
		`"reasoning_effort": "low", "thinking_budget": 512`: 512,
		`"reasoning_effort": "high", "thinking_budget": 0`:  0,
	}
	for fields, want := range cases {
		p, err := toGemini([]byte(`{"messages": [], ` + fields + `}`))
		if err != nil {
			t.Fatalf("%s: %v", fields, err)
		}
		tc := p.GenerationConfig.ThinkingConfig
		if tc == nil || tc.Budget == nil || *tc.Budget != want {
			t.Errorf("%s: thinkingConfig = %+v, want budget %d", fields, tc, want)
		}
	}

	p, err := toGemini([]byte(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.GenerationConfig.ThinkingConfig != nil {
		t.Errorf("unset effort produced thinkingConfig %+v; want the field absent", p.GenerationConfig.ThinkingConfig)
	}
}

func TestToGeminiConvertsAudioParts(t *testing.T) {
	p, err := toGemini([]byte(`{"messages":[
		{"role":"user","content":[
			{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}},
			{"type":"input_audio","input_audio":{"data":"BBBB","format":"mp3"}},
			{"type":"text","text":"transcribe"}
		]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Contents) != 1 || len(p.Contents[0].Parts) != 3 {
		t.Fatalf("parts = %+v, want three", p.Contents)
	}
	if m := p.Contents[0].Parts[0].InlineData.MimeType; m != "audio/wav" {
		t.Errorf("wav mime = %q", m)
	}
	if m := p.Contents[0].Parts[1].InlineData.MimeType; m != "audio/mpeg" {
		t.Errorf("mp3 mime = %q, want the canonical audio/mpeg", m)
	}
}

// Video rides in as a data URI on an image_url part; the payload must be
// forwarded with its video MIME type intact.
func TestToGeminiConvertsVideoDataURI(t *testing.T) {
	p, err := toGemini([]byte(`{"messages":[
		{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"data:video/mp4;base64,AAAA"}},
			{"type":"text","text":"what happens"}
		]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	part := p.Contents[0].Parts[0]
	if part.InlineData == nil || part.InlineData.MimeType != "video/mp4" {
		t.Errorf("video part = %+v", part)
	}
}

// A non-thinking upstream preserves thinking controls only if a proxy-side
// capability check allows them; the error must surface as the upstream's 400
// rather than being retried against sibling slots.
func TestGeminiThinkingOnNonThinkingModelPassesThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Model does not support thinkingConfig","status":"INVALID_ARGUMENT"}}`))
	}))
	defer srv.Close()

	a := NewGeminiAdapter(srv.URL, nil, newTestClient())
	_, err := a.Complete(context.Background(), Request{
		Body:   []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`),
		Model:  "gemini/gemma-3-12b",
		APIKey: "k",
	})
	endpoint, ok := err.(*EndpointError)
	if !ok || endpoint.StatusCode != http.StatusBadRequest || !strings.Contains(string(endpoint.Body), "thinkingConfig") {
		t.Fatalf("err = %v, want the upstream 400 passed through", err)
	}
}
