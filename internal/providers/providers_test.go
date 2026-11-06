package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient points adapters at a local test server.
func newTestClient() *Client { return NewClient() }

// --- body rewriting ---

func TestRewriteModelPreservesUnknownFields(t *testing.T) {
	// Provider-specific extensions the proxy does not model must survive; the
	// proxy must never re-encode a request body it does not understand.
	body := []byte(`{"model":"openai/gpt-4","messages":[{"role":"user","content":"hi"}],"provider_specific":{"x":1},"temperature":0.5}`)
	out, err := rewriteModel(body, "gpt-4")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["model"] != "gpt-4" {
		t.Errorf("model = %v, want gpt-4", fields["model"])
	}
	if _, ok := fields["provider_specific"]; !ok {
		t.Error("provider_specific field was dropped")
	}
	if fields["temperature"] != 0.5 {
		t.Errorf("temperature = %v, want 0.5", fields["temperature"])
	}
	if len(fields["messages"].([]any)) != 1 {
		t.Error("messages were not preserved")
	}
}

func TestRewriteModelRejectsInvalidJSON(t *testing.T) {
	if _, err := rewriteModel([]byte(`{invalid`), "gpt-4"); err == nil {
		t.Error("expected an error for malformed JSON")
	}
}

func TestHasMediaInput(t *testing.T) {
	cases := []struct {
		name              string
		body              string
		image, audio, vid bool
	}{
		{"image data URI", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}]}`, true, false, false},
		// Video rides on an image_url part but is not an image input: a
		// media allow-list must be able to permit one without the other.
		{"video data URI", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:video/mp4;base64,x"}}]}]}`, false, false, true},
		{"remote image URL", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`, true, false, false},
		{"responses-style image string", `{"messages":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,x"}]}]}`, true, false, false},
		// A malformed part must not panic the gate; it still counts as an
		// image part, so the model gate can reject it.
		{"image part without a URL", `{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`, true, false, false},
		{"audio part", `{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"a","format":"wav"}}]}]}`, false, true, false},
		{"empty audio part", `{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"format":"wav"}}]}]}`, false, false, false},
		{"plain text", `{"messages":[{"role":"user","content":"plain text"}]}`, false, false, false},
		{"text part", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`, false, false, false},
		{"no messages", `{"model":"gpt-4"}`, false, false, false},
		{"malformed", `{oops`, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			image, audio, vid := HasMediaInput([]byte(tc.body))
			if image != tc.image || audio != tc.audio || vid != tc.vid {
				t.Errorf("HasMediaInput(%s) = %v/%v/%v, want %v/%v/%v",
					tc.body, image, audio, vid, tc.image, tc.audio, tc.vid)
			}
		})
	}
}

// --- OpenAI parameter normalisation ---

func TestRequiresMaxCompletionTokens(t *testing.T) {
	for _, model := range []string{"gpt-5", "gpt-5-mini", "o3-mini", "o4-mini"} {
		if !requiresMaxCompletionTokens(model) {
			t.Errorf("%s should use max_completion_tokens", model)
		}
	}
	for _, model := range []string{"gpt-4", "gpt-4o", "o1-preview", "gpt-3.5-turbo"} {
		if requiresMaxCompletionTokens(model) {
			t.Errorf("%s should keep max_tokens", model)
		}
	}
}

func TestNormalizeParamsRenamesMaxTokens(t *testing.T) {
	a := &OpenAIAdapter{}
	body := []byte(`{"model":"gpt-5","max_tokens":100}`)
	out, err := a.normalizeParams(body, "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	_ = json.Unmarshal(out, &fields)
	if _, ok := fields["max_tokens"]; ok {
		t.Error("max_tokens should have been renamed")
	}
	if fields["max_completion_tokens"] != float64(100) {
		t.Errorf("max_completion_tokens = %v, want 100", fields["max_completion_tokens"])
	}
}

func TestNormalizeParamsDoesNotClobberExplicitField(t *testing.T) {
	a := &OpenAIAdapter{}
	body := []byte(`{"model":"gpt-5","max_tokens":100,"max_completion_tokens":7}`)
	out, err := a.normalizeParams(body, "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	_ = json.Unmarshal(out, &fields)
	if fields["max_completion_tokens"] != float64(7) {
		t.Errorf("max_completion_tokens = %v, want the caller's explicit 7", fields["max_completion_tokens"])
	}
}

func TestNormalizeParamsLeavesOtherModelsAlone(t *testing.T) {
	a := &OpenAIAdapter{}
	body := []byte(`{"model":"gpt-4","max_tokens":100}`)
	out, err := a.normalizeParams(body, "gpt-4")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	_ = json.Unmarshal(out, &fields)
	if fields["max_tokens"] != float64(100) {
		t.Error("gpt-4 should keep max_tokens")
	}
}

// --- reasoning-block stripping ---

func TestStripReasoningBlocks(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"<think>secret</think>  visible  "}}]}`)
	out := stripReasoningBlocks(body)
	if strings.Contains(string(out), "secret") {
		t.Error("reasoning block was not removed")
	}
	if !strings.Contains(string(out), "visible") {
		t.Error("visible content was removed")
	}
}

func TestStripReasoningBlocksLeavesCleanBodyIntact(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"plain"}}]}`)
	if string(stripReasoningBlocks(body)) != string(body) {
		t.Error("an unmodified body should round-trip unchanged")
	}
}

func TestStripReasoningBlocksIgnoresNonChatBodies(t *testing.T) {
	for _, body := range []string{`{}`, `{"choices":[]}`, `not json`, `{"object":"embedding"}`} {
		if got := string(stripReasoningBlocks([]byte(body))); got != body {
			t.Errorf("body %q was modified to %q", body, got)
		}
	}
}

// --- Anthropic request translation ---

func TestToAnthropicHoistsSystemPrompt(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"hello"}
	],"max_tokens":64}`)
	out, err := toAnthropic(body, "anthropic/claude-3-5-sonnet-20241022")
	if err != nil {
		t.Fatal(err)
	}
	if got := systemText(out.System); got != "be brief" {
		t.Errorf("system = %q, want 'be brief'", got)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 (system is hoisted)", len(out.Messages))
	}
	if out.Messages[0].Role != "user" {
		t.Errorf("role = %q, want user", out.Messages[0].Role)
	}
	if out.Model != "claude-3-5-sonnet-20241022" {
		t.Errorf("model = %q, want the provider prefix stripped", out.Model)
	}
	if out.MaxTokens != 64 {
		t.Errorf("maxTokens = %d, want 64", out.MaxTokens)
	}
}

func TestToAnthropicDefaultsMaxTokens(t *testing.T) {
	// Anthropic requires max_tokens; the OpenAI shape does not.
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out, err := toAnthropic(body, "anthropic/claude")
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxTokens <= 0 {
		t.Errorf("maxTokens = %d, want a positive default", out.MaxTokens)
	}
}

func TestToAnthropicConvertsImageDataURI(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"text","text":"describe"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}
	]}],"max_tokens":10}`)
	out, err := toAnthropic(body, "anthropic/claude")
	if err != nil {
		t.Fatal(err)
	}
	parts, ok := out.Messages[0].Content.([]contentPart)
	if !ok {
		t.Fatalf("content type = %T, want []contentPart", out.Messages[0].Content)
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(parts))
	}
	if parts[1].Source == nil {
		t.Fatal("image part has no source")
	}
	if parts[1].Source.MediaType != "image/png" {
		t.Errorf("mediaType = %q, want image/png", parts[1].Source.MediaType)
	}
	if parts[1].Source.Data != "QUJD" {
		t.Errorf("data = %q, want QUJD", parts[1].Source.Data)
	}
}

// The responses shape names the part input_image and sends the URL as a bare
// string. Both must reach Anthropic as media: a part that fails to decode
// takes the message's text down with it and the caller is told nothing.
func TestToAnthropicConvertsResponsesStyleImage(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"text","text":"describe"},
		{"type":"input_image","image_url":"data:image/png;base64,QUJD"}
	]}],"max_tokens":10}`)
	out, err := toAnthropic(body, "anthropic/claude")
	if err != nil {
		t.Fatal(err)
	}
	parts, ok := out.Messages[0].Content.([]contentPart)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %#v, want the text and image parts", out.Messages[0].Content)
	}
	if parts[0].Text != "describe" {
		t.Errorf("text part = %q", parts[0].Text)
	}
	if parts[1].Source == nil || parts[1].Source.Data != "QUJD" {
		t.Fatalf("image part = %#v, want the base64 payload", parts[1])
	}
}

// max_completion_tokens is what the newer OpenAI clients send; ignoring it
// silently fell back to the default.
func TestToAnthropicHonorsMaxCompletionTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"new field wins", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":10,"max_completion_tokens":77}`, 77},
		{"new field alone", `{"messages":[{"role":"user","content":"hi"}],"max_completion_tokens":33}`, 33},
		{"legacy field", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":12}`, 12},
	}
	for _, tc := range cases {
		out, err := toAnthropic([]byte(tc.body), "anthropic/claude")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if out.MaxTokens != tc.want {
			t.Errorf("%s: maxTokens = %d, want %d", tc.name, out.MaxTokens, tc.want)
		}
	}
}

func TestSplitDataURI(t *testing.T) {
	media, data, ok := splitDataURI("data:image/jpeg;base64,ZZZ")
	if !ok || media != "image/jpeg" || data != "ZZZ" {
		t.Errorf("got (%q, %q, %v), want (image/jpeg, ZZZ, true)", media, data, ok)
	}
	if _, _, ok := splitDataURI("https://example.com/a.png"); ok {
		t.Error("a plain URL is not a data URI")
	}
	if _, _, ok := splitDataURI("data:image/png,ZZZ"); ok {
		t.Error("a non-base64 data URI should be rejected")
	}
	if _, _, ok := splitDataURI("data:;base64,ZZZ"); ok {
		t.Error("a data URI with no media type should be rejected")
	}
}

func TestMapStopReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"max_tokens":    "length",
		"stop_sequence": "stop",
		"tool_use":      "tool_calls",
		"":              "stop",
		"unknown":       "stop",
	}
	for in, want := range cases {
		if got := mapStopReason(in); got != want {
			t.Errorf("mapStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- Anthropic streaming, the path broken in Python ---

func TestAnthropicStreamProducesOpenAIChunks(t *testing.T) {
	// Anthropic sends typed events; the adapter must emit OpenAI chunks and a
	// [DONE] sentinel, and report usage accumulated across two events.
	events := strings.Join([]string{
		`event: message_start
data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","usage":{"input_tokens":12}}}`,
		``,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		``,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		``,
		`event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":7}}`,
		``,
		`event: message_stop
data: {"type":"message_stop"}`,
		``,
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, events)
	}))
	defer srv.Close()

	a := NewAnthropicAdapter(srv.URL, nil, newTestClient())
	stream, err := a.Stream(context.Background(), Request{
		Body:   []byte(`{"model":"anthropic/claude","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		Model:  "anthropic/claude",
		APIKey: "k",
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	var usage *Usage
	sawDone := false

	for {
		chunk, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if chunk.Done {
			sawDone = true
			continue
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if len(chunk.Data) == 0 {
			continue
		}
		var out struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(chunk.Data, &out); err != nil {
			t.Fatalf("decode chunk %s: %v", chunk.Data, err)
		}
		if out.Object != "chat.completion.chunk" {
			t.Errorf("object = %q, want chat.completion.chunk", out.Object)
		}
		for _, c := range out.Choices {
			text.WriteString(c.Delta.Content)
		}
	}

	if text.String() != "Hello" {
		t.Errorf("assembled text = %q, want Hello", text.String())
	}
	if !sawDone {
		t.Error("stream never produced a terminating chunk")
	}
	if usage == nil {
		t.Fatal("no usage reported")
	}
	if usage.PromptTokens != 12 || usage.CompletionTokens != 7 || usage.TotalTokens != 19 {
		t.Errorf("usage = %+v, want prompt 12 completion 7 total 19", usage)
	}
}

func TestAnthropicStreamSkipsPingEvents(t *testing.T) {
	events := ": ping\n\n" +
		`event: content_block_delta` + "\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}` + "\n\n" +
		`event: message_stop` + "\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	s := &anthropicStream{inner: newSSEStream(io.NopCloser(strings.NewReader(events))), model: "m"}
	defer s.Close()

	chunk, err := s.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if chunk.Done {
		t.Fatal("expected a content chunk, got the terminator")
	}
}

// --- Anthropic buffered completion ---

func TestAnthropicCompleteTranslatesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the request was translated into Anthropic's shape.
		var sent map[string]any
		_ = json.NewDecoder(r.Body).Decode(&sent)
		if sent["model"] != "claude-3-5-sonnet-20241022" {
			t.Errorf("upstream model = %v, want the prefix stripped", sent["model"])
		}
		if systemJSON, _ := json.Marshal(sent["system"]); !strings.Contains(string(systemJSON), "be terse") {
			t.Errorf("system = %s, want the hoisted system prompt", systemJSON)
		}
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("x-api-key = %q, want test-key", got)
		}
		if got := r.Header.Get("anthropic-version"); got == "" {
			t.Error("anthropic-version header missing")
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","role":"assistant","stop_reason":"end_turn",
			"content":[{"type":"text","text":"hi there"}],
			"usage":{"input_tokens":5,"output_tokens":3}}`)
	}))
	defer srv.Close()

	a := NewAnthropicAdapter(srv.URL, nil, newTestClient())
	resp, err := a.Complete(context.Background(), Request{
		Body:   []byte(`{"model":"anthropic/claude-3-5-sonnet-20241022","messages":[{"role":"system","content":"be terse"},{"role":"user","content":"hi"}],"max_tokens":32}`),
		Model:  "anthropic/claude-3-5-sonnet-20241022",
		APIKey: "test-key",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", out.Object)
	}
	if out.Choices[0].Message.Content != "hi there" {
		t.Errorf("content = %q, want 'hi there'", out.Choices[0].Message.Content)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", out.Choices[0].FinishReason)
	}
	if out.Usage.TotalTokens != 8 {
		t.Errorf("total_tokens = %d, want 8", out.Usage.TotalTokens)
	}
}

// --- Gemini translation ---

func TestToGeminiMapsRoles(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"be nice"},
		{"role":"user","content":"hello"},
		{"role":"assistant","content":"hi"}
	],"max_tokens":50}`)
	out, err := toGemini(body)
	if err != nil {
		t.Fatal(err)
	}
	if out.SystemInstruction == nil || out.SystemInstruction.Parts[0].Text != "be nice" {
		t.Error("system prompt was not hoisted into systemInstruction")
	}
	if len(out.Contents) != 2 {
		t.Fatalf("contents = %d, want 2", len(out.Contents))
	}
	if out.Contents[0].Role != "user" {
		t.Errorf("first role = %q, want user", out.Contents[0].Role)
	}
	// Gemini calls the assistant role "model".
	if out.Contents[1].Role != "model" {
		t.Errorf("second role = %q, want model", out.Contents[1].Role)
	}
}

// A responses-style input_image part must survive the Gemini translation; the
// media gate reports it as an image, so dropping it would send the model a
// request with no content at all.
func TestToGeminiConvertsResponsesStyleImage(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"text","text":"what is this"},
		{"type":"input_image","image_url":"data:image/png;base64,QUJD"}
	]}]}`)
	out, err := toGemini(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Contents) != 1 || len(out.Contents[0].Parts) != 2 {
		t.Fatalf("contents = %+v, want the text and image parts", out.Contents)
	}
	if got := out.Contents[0].Parts[0].Text; got != "what is this" {
		t.Errorf("text part = %q", got)
	}
	part := out.Contents[0].Parts[1]
	if part.InlineData == nil || part.InlineData.MimeType != "image/png" || part.InlineData.Data != "QUJD" {
		t.Fatalf("image part = %+v, want the inline PNG payload", part)
	}
}

func TestToGeminiHonorsMaxCompletionTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"new field wins", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":10,"max_completion_tokens":77}`, 77},
		{"new field alone", `{"messages":[{"role":"user","content":"hi"}],"max_completion_tokens":33}`, 33},
		{"legacy field", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":12}`, 12},
		{"neither", `{"messages":[{"role":"user","content":"hi"}]}`, 4096},
	}
	for _, tc := range cases {
		out, err := toGemini([]byte(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := out.GenerationConfig.MaxOutputTokens; got != tc.want {
			t.Errorf("%s: maxOutputTokens = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestGeminiCompleteTranslatesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/models/gemini-1.5-pro:generateContent") {
			t.Errorf("path = %q, want the model in the path", r.URL.Path)
		}
		if got := r.URL.Query().Get("key"); got != "gkey" {
			t.Errorf("key query param = %q, want gkey", got)
		}
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}],
			"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":6,"totalTokenCount":10}}`)
	}))
	defer srv.Close()

	a := NewGeminiAdapter(srv.URL, nil, newTestClient())
	resp, err := a.Complete(context.Background(), Request{
		Body:   []byte(`{"model":"gemini/gemini-1.5-pro","messages":[{"role":"user","content":"hi"}]}`),
		Model:  "gemini/gemini-1.5-pro",
		APIKey: "gkey",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Choices[0].Message.Content != "hello" {
		t.Errorf("content = %q, want hello", out.Choices[0].Message.Content)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", out.Choices[0].FinishReason)
	}
	if out.Usage.TotalTokens != 10 {
		t.Errorf("total_tokens = %d, want 10", out.Usage.TotalTokens)
	}
}

func TestGeminiStreamProducesChunks(t *testing.T) {
	events := `data: {"candidates":[{"content":{"parts":[{"text":"Hi"}]}}]}` + "\n\n" +
		`data: {"candidates":[{"content":{"parts":[{"text":" there"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, events)
	}))
	defer srv.Close()

	a := NewGeminiAdapter(srv.URL, nil, newTestClient())
	stream, err := a.Stream(context.Background(), Request{
		Body:  []byte(`{"model":"gemini/g","messages":[{"role":"user","content":"hi"}]}`),
		Model: "gemini/g", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	var usage *Usage
	for {
		chunk, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if chunk.Done {
			continue
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if len(chunk.Data) == 0 {
			continue
		}
		var out struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		_ = json.Unmarshal(chunk.Data, &out)
		for _, c := range out.Choices {
			text.WriteString(c.Delta.Content)
		}
	}
	if text.String() != "Hi there" {
		t.Errorf("assembled text = %q, want 'Hi there'", text.String())
	}
	if usage == nil || usage.TotalTokens != 5 {
		t.Errorf("usage = %+v, want total 5", usage)
	}
}

// --- registry ---

func TestRegistryBuiltins(t *testing.T) {
	r := NewRegistry(nil)
	for _, slug := range []string{"openai", "anthropic", "gemini"} {
		if !r.Known(slug) {
			t.Errorf("%s should be a built-in provider", slug)
		}
	}
}

func TestRegistryUnknownSlugGetsOpenAIAdapter(t *testing.T) {
	// The whole point: an unknown provider must still work through the
	// OpenAI-compatible path, so adding Groq needs no rebuild.
	r := NewRegistry(nil)
	a := r.Adapter("groq")
	if _, ok := a.(*OpenAIAdapter); !ok {
		t.Errorf("adapter type = %T, want *OpenAIAdapter", a)
	}
}

func TestRegistryDefineRuntimeProvider(t *testing.T) {
	r := NewRegistry(nil)
	err := r.Define(Definition{
		Slug:     "my-llm",
		BaseURL:  "https://llm.internal/v1",
		Protocol: ProtocolOpenAI,
	})
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	if !r.Known("my-llm") {
		t.Error("runtime-defined provider should resolve")
	}
	spec, err := r.Lookup("my-llm")
	if err != nil {
		t.Fatal(err)
	}
	if spec.BaseURL != "https://llm.internal/v1" {
		t.Errorf("baseURL = %q", spec.BaseURL)
	}
	r.Remove("my-llm")
	if r.Known("my-llm") {
		t.Error("Remove did not take effect")
	}
}

func TestRegistryDefineAnthropicProtocolNoRebuild(t *testing.T) {
	// A provider speaking Anthropic's protocol is configured, not compiled.
	r := NewRegistry(nil)
	if err := r.Define(Definition{
		Slug: "bedrock-anthropic", BaseURL: "https://proxy.internal/v1", Protocol: ProtocolAnthropic,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Adapter("bedrock-anthropic").(*AnthropicAdapter); !ok {
		t.Error("expected the Anthropic adapter for an anthropic-protocol provider")
	}
}

func TestRegistryDefineRejectsBadInput(t *testing.T) {
	r := NewRegistry(nil)
	if err := r.Define(Definition{Slug: "x"}); err == nil {
		t.Error("expected an error for a missing base URL")
	}
	if err := r.Define(Definition{Slug: "x", BaseURL: "ftp://nope"}); err == nil {
		t.Error("expected an error for a non-http scheme")
	}
	if err := r.Define(Definition{Slug: "", BaseURL: "https://x"}); err == nil {
		t.Error("expected an error for a missing slug")
	}
	if err := r.Define(Definition{Slug: "x", BaseURL: "https://x", Protocol: "martian"}); err == nil {
		t.Error("expected an error for an unknown protocol")
	}
}

func TestRegistryBuiltinNotShadowedByDefinition(t *testing.T) {
	r := NewRegistry(nil)
	// A definition must never silently change a compiled provider's behaviour.
	if err := r.Define(Definition{Slug: "anthropic", BaseURL: "https://evil.internal", Protocol: ProtocolOpenAI}); err != nil {
		t.Fatal(err)
	}
	spec, err := r.Lookup("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Protocol != ProtocolAnthropic {
		t.Errorf("protocol = %s, want anthropic; built-ins take precedence", spec.Protocol)
	}
}

func TestAdapterForBaseURLOverride(t *testing.T) {
	r := NewRegistry(nil)
	a := r.AdapterFor("groq", "https://custom.groq.example/openai/v1")
	oa, ok := a.(*OpenAIAdapter)
	if !ok {
		t.Fatalf("adapter type = %T", a)
	}
	if oa.BaseURL != "https://custom.groq.example/openai/v1" {
		t.Errorf("baseURL = %q, want the override applied", oa.BaseURL)
	}
}

func TestModelsURL(t *testing.T) {
	cases := map[string]string{
		"https://api.openai.com/v1":       "https://api.openai.com/v1/models",
		"https://api.groq.com/openai/v1/": "https://api.groq.com/openai/v1/models",
		"https://example.com":             "https://example.com/v1/models",
	}
	for in, want := range cases {
		if got := modelsURL(in); got != want {
			t.Errorf("modelsURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateBaseURL(t *testing.T) {
	if err := validateBaseURL("https://ok.example/v1"); err != nil {
		t.Errorf("valid URL rejected: %v", err)
	}
	for _, bad := range []string{"", "not a url", "ftp://x", "https://", "://x"} {
		if err := validateBaseURL(bad); err == nil {
			t.Errorf("validateBaseURL(%q) accepted an invalid URL", bad)
		}
	}
}

// --- error classification ---

func TestEndpointErrorTruncatesBody(t *testing.T) {
	e := &EndpointError{StatusCode: 500, Body: []byte(strings.Repeat("x", 500)), Provider: "openai"}
	if len(e.Error()) > 260 {
		t.Errorf("error message is %d chars; the body should be truncated", len(e.Error()))
	}
}

func TestUnsupportedCapabilitiesAreReported(t *testing.T) {
	ctx := context.Background()
	a := NewAnthropicAdapter("https://x", nil, nil)
	if _, err := a.Embeddings(ctx, Request{}); err == nil {
		t.Error("anthropic embeddings should be unsupported")
	}
	g := NewGeminiAdapter("https://x", nil, nil)
	if _, err := g.Speak(ctx, Request{}); err == nil {
		t.Error("gemini speech should be unsupported")
	}
}

// --- SSE parsing ---

func TestParseSSELine(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		valid bool
	}{
		{"data: {\"a\":1}", `{"a":1}`, true},
		{"data:{\"a\":1}", `{"a":1}`, true},
		{"event: message_start", "", false},
		{": keepalive", "", false},
		{"", "", false},
		{"   ", "", false},
		{"data: [DONE]", "[DONE]", true},
	}
	for _, tc := range cases {
		got, ok := parseSSELine([]byte(tc.in))
		if ok != tc.valid {
			t.Errorf("parseSSELine(%q) valid = %v, want %v", tc.in, ok, tc.valid)
			continue
		}
		if ok && string(got) != tc.want {
			t.Errorf("parseSSELine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExtractUsage(t *testing.T) {
	u := extractUsage([]byte(`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4}}`))
	if u == nil {
		t.Fatal("usage not extracted")
	}
	if u.PromptTokens != 3 || u.CompletionTokens != 4 {
		t.Errorf("usage = %+v", u)
	}
	// total_tokens is derived when the provider omits it.
	if u.TotalTokens != 7 {
		t.Errorf("totalTokens = %d, want 7 (derived)", u.TotalTokens)
	}
	if extractUsage([]byte(`{"choices":[]}`)) != nil {
		t.Error("usage should be nil when absent")
	}
}

func TestOpenAIStreamPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"+
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n"+
			"data: [DONE]\n\n")
	}))
	defer srv.Close()

	a := NewOpenAIAdapter(srv.URL, AuthHeaderNames{}, newTestClient())
	stream, err := a.Stream(context.Background(), Request{
		Body:  []byte(`{"model":"gpt-4","stream":true}`),
		Model: "openai/gpt-4", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var sawUsage, sawDone bool
	for {
		c, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if c.Done {
			sawDone = true
		}
		if c.Usage != nil {
			sawUsage = true
		}
	}
	if !sawUsage {
		t.Error("usage chunk was not surfaced")
	}
	if !sawDone {
		t.Error("[DONE] was not surfaced")
	}
}

func TestOpenAIStreamRequiresStreamFlag(t *testing.T) {
	a := NewOpenAIAdapter("https://x", AuthHeaderNames{}, nil)
	if _, err := a.Stream(context.Background(), Request{
		Body:  []byte(`{"model":"gpt-4"}`),
		Model: "openai/gpt-4",
	}); err == nil {
		t.Error("expected an error when stream is not enabled in the body")
	}
}

// Gemini streams must use alt=sse so Google answers with server-sent events
// rather than its default chunked JSON array, which the SSE reader cannot
// parse.
func TestGeminiStreamRequestsSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("alt") != "sse" {
			t.Errorf("alt query param = %q, want sse", r.URL.Query().Get("alt"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}]}`)
		fmt.Fprintln(w, "data: [DONE]")
	}))
	defer srv.Close()

	a := NewGeminiAdapter(srv.URL, nil, newTestClient())
	stream, err := a.Stream(context.Background(), Request{
		Body: []byte(`{"messages":[{"role":"user","content":"hi"}]}`), Model: "gemini/g", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	for {
		_, err := stream.Next()
		if err != nil {
			break
		}
	}
}

// A cursor-split key must survive URL escaping so the request still resolves.
func TestGeminiEscapesKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("key"); got != "k&x=1" {
			t.Errorf("key query param = %q, want 'k&x=1'", got)
		}
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`)
	}))
	defer srv.Close()

	a := NewGeminiAdapter(srv.URL, nil, newTestClient())
	if _, err := a.Complete(context.Background(), Request{
		Body: []byte(`{"messages":[]}`), Model: "gemini/g", APIKey: "k&x=1",
	}); err != nil {
		t.Fatalf("Complete with a cursor-bearing key: %v", err)
	}
}

// The terminal streamed chunk must carry finish_reason even for a plain STOP,
// and stop-reason chunks must not be merged over text deltas.
func TestGeminiStreamSurfacesStopFinishReason(t *testing.T) {
	events := `data: {"candidates":[{"content":{"parts":[{"text":"Hi"}]}}]}` + "\n\n" +
		`data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, events)
	}))
	defer srv.Close()

	a := NewGeminiAdapter(srv.URL, nil, newTestClient())
	stream, err := a.Stream(context.Background(), Request{
		Body: []byte(`{"messages":[{"role":"user","content":"hi"}]}`), Model: "gemini/g", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var sawStop bool
	var sawText bool
	for {
		chunk, err := stream.Next()
		if err != nil {
			break
		}
		if len(chunk.Data) == 0 {
			continue
		}
		var out struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason any `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(chunk.Data, &out); err != nil {
			t.Fatal(err)
		}
		for _, c := range out.Choices {
			if c.Delta.Content != "" {
				sawText = true
			}
			if s, ok := c.FinishReason.(string); ok && s == "stop" {
				sawStop = true
			}
		}
	}
	if !sawText {
		t.Error("text delta was not surfaced")
	}
	if !sawStop {
		t.Error("finish_reason \"stop\" was not surfaced before [DONE]")
	}
}

// Gemini ends its SSE body without the OpenAI [DONE] sentinel, so the adapter
// has to synthesize it; usage must count thinking as completion tokens.
func TestGeminiStreamEndsWithDONE(t *testing.T) {
	events := `data: {"candidates":[{"content":{"parts":[{"text":"Hi"}]}}]}` + "\n\n" +
		`data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"thoughtsTokenCount":7,"totalTokenCount":12}}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, events)
	}))
	defer srv.Close()

	a := NewGeminiAdapter(srv.URL, nil, newTestClient())
	stream, err := a.Stream(context.Background(), Request{
		Body: []byte(`{"messages":[{"role":"user","content":"hi"}]}`), Model: "gemini/g", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var done bool
	var lastUsage *Usage
	for {
		chunk, err := stream.Next()
		if err != nil {
			if err != io.EOF {
				t.Fatalf("Next: %v", err)
			}
			break
		}
		if chunk.Done {
			done = true
			continue
		}
		if chunk.Usage != nil {
			lastUsage = chunk.Usage
		}
	}
	if !done {
		t.Fatal("stream ended without a Done chunk")
	}
	if lastUsage == nil {
		t.Fatal("final usage was not reported")
	}
	if lastUsage.CompletionTokens != 10 {
		t.Errorf("completion_tokens = %d, want 10 (candidates + thinking)", lastUsage.CompletionTokens)
	}
	if lastUsage.TotalTokens != 12 {
		t.Errorf("total_tokens = %d, want 12", lastUsage.TotalTokens)
	}
}

// Buffered Gemini usage must reconcile prompt+completion with total_tokens and
// expose thinking separately as reasoning_tokens.
func TestGeminiUsageCountsThinking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"391"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":15,"candidatesTokenCount":3,"thoughtsTokenCount":226,"totalTokenCount":244}}`)
	}))
	defer srv.Close()

	a := NewGeminiAdapter(srv.URL, nil, newTestClient())
	resp, err := a.Complete(context.Background(), Request{
		Body: []byte(`{"messages":[{"role":"user","content":"hi"}]}`), Model: "gemini/g", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var out struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
			Details          struct {
				Reasoning int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Usage.CompletionTokens != 229 {
		t.Errorf("completion_tokens = %d, want 229 (candidates + thinking)", out.Usage.CompletionTokens)
	}
	if out.Usage.PromptTokens+out.Usage.CompletionTokens != out.Usage.TotalTokens {
		t.Errorf("prompt %d + completion %d != total %d", out.Usage.PromptTokens, out.Usage.CompletionTokens, out.Usage.TotalTokens)
	}
	if out.Usage.Details.Reasoning != 226 {
		t.Errorf("reasoning_tokens = %d, want 226", out.Usage.Details.Reasoning)
	}
}

// Replayed tool calls need Gemini's thought signature: a real captured one is
// preserved, and a call that lost it gets the documented sentinel.
func TestToGeminiSignsReplayedToolCalls(t *testing.T) {
	body := `{"model":"gemini-3.5-flash-lite","messages":[` +
		`{"role":"user","content":"weather?"},` +
		`{"role":"assistant","content":null,"tool_calls":[` +
		`{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},` +
		`{"id":"call_2","type":"function","function":{"name":"get_time","arguments":"{}"},"thought_signature":"sig-abc"}` +
		`]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"21C"},` +
		`{"role":"tool","tool_call_id":"call_2","content":"noon"}]}`

	out, err := toGemini([]byte(body))
	if err != nil {
		t.Fatalf("toGemini: %v", err)
	}
	if len(out.Contents) != 3 {
		t.Fatalf("contents = %d, want 3 (user, model, merged tool results)", len(out.Contents))
	}
	parts := out.Contents[1].Parts
	if out.Contents[1].Role != "model" || len(parts) != 2 {
		t.Fatalf("model turn = %q with %d parts, want 2", out.Contents[1].Role, len(parts))
	}
	if parts[0].FunctionCall == nil || parts[0].FunctionCall.Name != "get_weather" {
		t.Fatalf("first part = %+v, want a get_weather call", parts[0])
	}
	if parts[0].ThoughtSignature != skipThoughtSignature {
		t.Errorf("unsigned call signature = %q, want %q", parts[0].ThoughtSignature, skipThoughtSignature)
	}
	if parts[1].ThoughtSignature != "sig-abc" {
		t.Errorf("echoed signature = %q, want sig-abc", parts[1].ThoughtSignature)
	}
}

func TestGeminiResponseExposesThoughtSignature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}},"thoughtSignature":"sig-xyz"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`)
	}))
	defer srv.Close()

	a := NewGeminiAdapter(srv.URL, nil, newTestClient())
	resp, err := a.Complete(context.Background(), Request{
		Body: []byte(`{"messages":[{"role":"user","content":"hi"}]}`), Model: "gemini/g", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var out struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID               string `json:"id"`
					ThoughtSignature string `json:"thought_signature"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices) == 0 || len(out.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v, want one", out.Choices)
	}
	if got := out.Choices[0].Message.ToolCalls[0].ThoughtSignature; got != "sig-xyz" {
		t.Errorf("thought_signature = %q, want sig-xyz", got)
	}
}

func TestGeminiStreamCarriesToolSignature(t *testing.T) {
	events := `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}},"thoughtSignature":"sig-stream"}]}}]}` + "\n\n" +
		`data: {"candidates":[{"finishReason":"STOP"}]}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, events)
	}))
	defer srv.Close()

	a := NewGeminiAdapter(srv.URL, nil, newTestClient())
	stream, err := a.Stream(context.Background(), Request{
		Body: []byte(`{"messages":[{"role":"user","content":"hi"}]}`), Model: "gemini/g", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var signature string
	for {
		chunk, err := stream.Next()
		if err != nil {
			break
		}
		if len(chunk.Data) == 0 {
			continue
		}
		var out struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
						ThoughtSignature string `json:"thought_signature"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(chunk.Data, &out); err != nil {
			t.Fatal(err)
		}
		for _, c := range out.Choices {
			for _, tc := range c.Delta.ToolCalls {
				if tc.ThoughtSignature != "" {
					signature = tc.ThoughtSignature
				}
			}
		}
	}
	if signature != "sig-stream" {
		t.Errorf("streamed thought_signature = %q, want sig-stream", signature)
	}
}

// Gemini 3 rejects thinkingBudget 0, so a "none" effort must drop the knob
// rather than turn a valid request into an upstream 400.
func TestGeminiDropsZeroThinkingForGemini3(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`)

	payload, err := toGemini(body)
	if err != nil {
		t.Fatalf("toGemini: %v", err)
	}
	if payload.GenerationConfig.ThinkingConfig == nil || payload.GenerationConfig.ThinkingConfig.Budget == nil || *payload.GenerationConfig.ThinkingConfig.Budget != 0 {
		t.Fatalf("thinking config = %+v, want a zero budget", payload.GenerationConfig.ThinkingConfig)
	}
	dropUnsupportedZeroThinking(payload, "gemini/gemini-3.5-flash-lite")
	if payload.GenerationConfig.ThinkingConfig != nil {
		t.Error("Gemini 3 must not receive thinkingBudget 0")
	}

	payload, err = toGemini(body)
	if err != nil {
		t.Fatalf("toGemini: %v", err)
	}
	dropUnsupportedZeroThinking(payload, "gemini/gemini-2.5-flash-lite")
	if payload.GenerationConfig.ThinkingConfig == nil {
		t.Error("Gemini 2.5 Flash supports disabling thinking and must keep the zero budget")
	}
}

// A requested embedding width must reach Gemini as outputDimensionality;
// silently returning the default width breaks vector stores.
func TestGeminiEmbeddingsForwardsDimensions(t *testing.T) {
	var got struct {
		Requests []struct {
			Model                string `json:"model"`
			OutputDimensionality int    `json:"outputDimensionality"`
		} `json:"requests"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		fmt.Fprint(w, `{"embeddings":[{"values":[0.1,0.2]},{"values":[0.3,0.4]}]}`)
	}))
	defer srv.Close()

	a := NewGeminiAdapter(srv.URL, nil, newTestClient())
	resp, err := a.Embeddings(context.Background(), Request{
		Body:  []byte(`{"model":"gemini-embedding-001","input":["a","b"],"dimensions":768}`),
		Model: "gemini-embedding-001", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("Embeddings: %v", err)
	}
	if len(got.Requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(got.Requests))
	}
	for i, req := range got.Requests {
		if req.OutputDimensionality != 768 {
			t.Errorf("requests[%d].outputDimensionality = %d, want 768", i, req.OutputDimensionality)
		}
		if req.Model != "models/gemini-embedding-001" {
			t.Errorf("requests[%d].model = %q", i, req.Model)
		}
	}
	var out struct {
		Data []struct {
			Index int `json:"index"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 2 {
		t.Errorf("data = %d embeddings, want 2", len(out.Data))
	}
}

// The caller's breakpoints must not be exceeded: Anthropic caps them at four.
func TestAnthropicCacheBreakpointCap(t *testing.T) {
	parts := make([]contentPart, 4)
	for i := range parts {
		parts[i] = contentPart{Type: "text", Text: "x", CacheControl: &cacheControl{Type: "ephemeral"}}
	}
	req := &anthropicRequest{
		System:   []contentPart{{Type: "text", Text: "sys"}},
		Messages: []anthropicMessage{{Role: "user", Content: parts}},
	}
	req.injectCacheBreakpoints()
	if got := req.cacheBreakpointCount(); got != 4 {
		t.Errorf("breakpoints = %d, want 4 (cap respected)", got)
	}
	if system, ok := req.System.([]contentPart); ok && system[0].CacheControl != nil {
		t.Error("a fifth breakpoint was injected past the cap")
	}
}

// --- Anthropic agent parity: tools, thinking and prompt caching ---

// systemText flattens the system field, which is a string only until the
// adapter injects its cache breakpoint.
func systemText(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []contentPart:
		var b strings.Builder
		for _, p := range s {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// Tools, assistant tool_calls and tool results must survive the translation,
// and every tool_result must pair with the exact id the model issued.
func TestToAnthropicMapsToolsAndToolMessages(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"system","content":"be terse"},
			{"role":"user","content":"weather and time in Paris?"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},
				{"id":"call_2","type":"function","function":{"name":"get_time","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","content":"{\"temp_c\":21}"},
			{"role":"tool","tool_call_id":"call_2","content":"noon"}
		],
		"tools":[{"type":"function","function":{
			"name":"get_weather",
			"description":"current weather",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}
		}}],
		"tool_choice":"required"
	}`)
	out, err := toAnthropic(body, "anthropic/claude")
	if err != nil {
		t.Fatal(err)
	}

	if got := systemText(out.System); got != "be terse" {
		t.Fatalf("system = %q", got)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("tools = %+v, want one declaration", out.Tools)
	}
	decl := out.Tools[0]
	if decl.Name != "get_weather" || decl.Description != "current weather" {
		t.Errorf("declaration = %+v", decl)
	}
	// The caller's JSON Schema is forwarded intact through input_schema.
	if !strings.Contains(string(decl.InputSchema), `"additionalProperties":false`) {
		t.Errorf("inputSchema = %s, want the schema passed through", decl.InputSchema)
	}
	if out.ToolChoice == nil || out.ToolChoice.Type != "any" {
		t.Errorf("toolChoice = %+v, want any from tool_choice required", out.ToolChoice)
	}

	if len(out.Messages) != 3 {
		t.Fatalf("messages = %d, want user, assistant and merged tool results: %+v", len(out.Messages), out.Messages)
	}
	model, ok := out.Messages[1].Content.([]contentPart)
	if !ok || len(model) != 2 {
		t.Fatalf("assistant content = %#v, want two tool_use blocks", out.Messages[1].Content)
	}
	if call := model[0]; call.Type != "tool_use" || call.ID != "call_1" || call.Name != "get_weather" {
		t.Errorf("first tool_use = %+v", call)
	}
	if string(model[0].Input) != `{"city":"Paris"}` {
		t.Errorf("input = %s, want the argument object", model[0].Input)
	}

	toolTurn := out.Messages[2]
	if toolTurn.Role != "user" {
		t.Fatalf("tool turn role = %q, want user", toolTurn.Role)
	}
	results, ok := toolTurn.Content.([]contentPart)
	if !ok || len(results) != 2 {
		t.Fatalf("tool content = %#v, want two tool_result blocks in one turn", toolTurn.Content)
	}
	if results[0].Type != "tool_result" || results[0].ToolUseID != "call_1" {
		t.Errorf("first tool_result = %+v, want the original call id", results[0])
	}
	if string(results[0].Content) != `"{\"temp_c\":21}"` {
		t.Errorf("result content = %s, want the JSON object as a string", results[0].Content)
	}
	if results[1].ToolUseID != "call_2" || string(results[1].Content) != `"noon"` {
		t.Errorf("second tool_result = %+v", results[1])
	}
}

// Reasoning effort becomes an Anthropic thinking budget, and the stable prefix
// gets a cache breakpoint unless the model opts out.
func TestToAnthropicThinkingAndCacheControl(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"stable prefix"},
		{"role":"user","content":"hi"}
	],"reasoning_effort":"high","temperature":0.2,"max_tokens":100}`)

	out, err := buildAnthropic(body, "anthropic/claude", anthropicConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Thinking == nil || out.Thinking.BudgetTokens != anthropicEffortHigh {
		t.Fatalf("thinking = %+v, want a %d token budget", out.Thinking, anthropicEffortHigh)
	}
	if out.MaxTokens <= out.Thinking.BudgetTokens {
		t.Errorf("maxTokens = %d, must exceed the thinking budget", out.MaxTokens)
	}
	if out.Temperature != nil {
		t.Error("temperature must be dropped when thinking is enabled")
	}
	system, ok := out.System.([]contentPart)
	if !ok || len(system) != 1 || system[0].CacheControl == nil || system[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("system = %#v, want a cached block", out.System)
	}
	first, _ := out.Messages[0].Content.([]contentPart)
	if len(first) == 0 || first[len(first)-1].CacheControl == nil {
		t.Fatalf("first message = %#v, want a cache breakpoint", first)
	}

	plain, err := buildAnthropic(body, "anthropic/claude", anthropicConfig{NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	if system, ok := plain.System.([]contentPart); ok {
		for _, p := range system {
			if p.CacheControl != nil {
				t.Error("cache: false must not inject a system breakpoint")
			}
		}
	}
	if first, ok := plain.Messages[0].Content.([]contentPart); ok {
		for _, p := range first {
			if p.CacheControl != nil {
				t.Error("cache: false must not inject a message breakpoint")
			}
		}
	}
}

// A buffered tool_use reply becomes an OpenAI assistant message with
// tool_calls and a null content, and cached reads are reported separately.
func TestAnthropicCompleteReturnsToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","role":"assistant","stop_reason":"tool_use","content":[
			{"type":"text","text":"checking"},
			{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}
		],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":4}}`)
	}))
	defer srv.Close()

	a := NewAnthropicAdapter(srv.URL, nil, newTestClient())
	resp, err := a.Complete(context.Background(), Request{
		Body:   []byte(`{"messages":[{"role":"user","content":"weather?"}]}`),
		Model:  "anthropic/claude",
		APIKey: "k",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	choice := out.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", choice.FinishReason)
	}
	if string(choice.Message.Content) != `"checking"` {
		t.Errorf("content = %s, want the text block", choice.Message.Content)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v, want one", choice.Message.ToolCalls)
	}
	call := choice.Message.ToolCalls[0]
	if call.ID != "toolu_1" || call.Type != "function" || call.Function.Name != "get_weather" {
		t.Errorf("call = %+v, want the id, type and name preserved", call)
	}
	if call.Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("arguments = %q, want the input object as a string", call.Function.Arguments)
	}
	if out.Usage.PromptTokens != 14 || out.Usage.TotalTokens != 19 {
		t.Errorf("usage = %+v, want prompt 14 (10 + 4 cached) total 19", out.Usage)
	}
	if out.Usage.PromptTokensDetails == nil || out.Usage.PromptTokensDetails.CachedTokens != 4 {
		t.Errorf("cached tokens = %+v, want 4", out.Usage.PromptTokensDetails)
	}
}

// Anthropic streams tool calls as content_block_start plus input_json_delta
// fragments; the adapter must reassemble them into OpenAI tool_calls deltas.
func TestAnthropicStreamEmitsToolCallDeltas(t *testing.T) {
	events := strings.Join([]string{
		`event: message_start`, `data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_read_input_tokens":4}}}`, ``,
		`event: content_block_start`, `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}`, ``,
		`event: content_block_stop`, `data: {"type":"content_block_stop","index":0}`, ``,
		`event: message_delta`, `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`, ``,
		`event: message_stop`, `data: {"type":"message_stop"}`, ``,
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, events)
	}))
	defer srv.Close()

	a := NewAnthropicAdapter(srv.URL, nil, newTestClient())
	stream, err := a.Stream(context.Background(), Request{
		Body:  []byte(`{"messages":[{"role":"user","content":"weather?"}],"stream":true}`),
		Model: "anthropic/claude", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var args, id, name string
	var index = -1
	var finish string
	var usage *Usage
	for {
		chunk, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if len(chunk.Data) == 0 {
			continue
		}
		var out struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason any `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(chunk.Data, &out); err != nil {
			t.Fatal(err)
		}
		for _, c := range out.Choices {
			for _, call := range c.Delta.ToolCalls {
				index = call.Index
				if call.ID != "" {
					id = call.ID
				}
				if call.Function.Name != "" {
					name = call.Function.Name
				}
				args += call.Function.Arguments
			}
			if reason, ok := c.FinishReason.(string); ok {
				finish = reason
			}
		}
	}

	if index != 0 || id != "toolu_1" || name != "get_weather" {
		t.Errorf("tool delta = index %d id %q name %q", index, id, name)
	}
	if args != `{"city":"Paris"}` {
		t.Errorf("assembled arguments = %q", args)
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", finish)
	}
	if usage == nil || usage.PromptTokens != 14 || usage.CompletionTokens != 5 || usage.CachedTokens != 4 {
		t.Errorf("usage = %+v, want prompt 14 (with 4 cached) and completion 5", usage)
	}
}

// An embeddings batch must become one upstream call with one requests[]
// element per input, not one call per document.
func TestGeminiEmbeddingsBatchesInputs(t *testing.T) {
	var calls, requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, ":batchEmbedContents") {
			t.Errorf("path = %q, want batchEmbedContents", r.URL.Path)
		}
		calls++
		var sent struct {
			Requests []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"requests"`
		}
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = len(sent.Requests)
		var out struct {
			Embeddings []map[string]any `json:"embeddings"`
		}
		for range sent.Requests {
			out.Embeddings = append(out.Embeddings, map[string]any{"values": []float64{0.1, 0.2}})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	a := NewGeminiAdapter(srv.URL, nil, newTestClient())
	resp, err := a.Embeddings(context.Background(), Request{
		Body:   []byte(`{"model":"e","input":["one","two","three"]}`),
		Model:  "gemini/text-embedding-004",
		APIKey: "k",
	})
	if err != nil {
		t.Fatalf("Embeddings: %v", err)
	}
	if calls != 1 || requests != 3 {
		t.Fatalf("calls = %d with %d requests, want one call with three", calls, requests)
	}

	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 3 {
		t.Fatalf("embeddings = %d, want 3", len(out.Data))
	}
	for i, e := range out.Data {
		if e.Index != i || len(e.Embedding) != 2 {
			t.Errorf("embedding %d = %+v", i, e)
		}
	}
}

// A single SSE line larger than the old 4 MiB cap must survive: large tool
// results arrive as one frame.
func TestSSEOversizedFrameReads(t *testing.T) {
	big := strings.Repeat("x", 5<<20)
	s := newSSEStream(io.NopCloser(bytes.NewReader([]byte("data: " + big + "\n\n"))))
	defer s.Close()

	chunk, err := s.Next()
	if err != nil {
		t.Fatalf("5 MiB frame: %v", err)
	}
	if len(chunk.Data) != len(big) {
		t.Errorf("payload length = %d, want %d", len(chunk.Data), len(big))
	}
}
