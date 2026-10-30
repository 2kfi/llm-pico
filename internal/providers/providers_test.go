package providers

import (
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

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

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

func TestHasImageInput(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"plain text", `{"messages":[{"role":"user","content":"hello"}]}`, false},
		{"image part", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`, true},
		{"image later in conversation", `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":""},{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x/y.png"}}]}]}`, true},
		{"input_image part", `{"messages":[{"role":"user","content":[{"type":"input_image","image_url":"x"}]}]}`, true},
		{"no messages", `{"model":"gpt-4"}`, false},
		{"malformed", `{oops`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasImageInput([]byte(tc.body)); got != tc.want {
				t.Errorf("HasImageInput = %v, want %v", got, tc.want)
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
	if out.System != "be brief" {
		t.Errorf("system = %q, want 'be brief'", out.System)
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
		if sent["system"] != "be terse" {
			t.Errorf("system = %v, want the hoisted system prompt", sent["system"])
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
