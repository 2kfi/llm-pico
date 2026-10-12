package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AnthropicBaseURL is the upstream used when a model configures no api_base.
const AnthropicBaseURL = "https://api.anthropic.com/v1"

// AnthropicAdapter translates between the OpenAI chat format and Anthropic's
// Messages API.
//
// Both directions are lossy in places: Anthropic has no OpenAI-equivalent
// response envelope, and its streaming protocol is a typed event sequence
// rather than OpenAI's delta chunks. The adapter converts events into
// chat.completion.chunk frames as they arrive, so the proxy's streaming path
// is identical for every provider.
//
// The Python implementation overrode proxy_stream to return a
// (chunks, usage) tuple rather than an async generator, so the generic
// streaming loop raised TypeError for Anthropic. The Stream interface here
// makes that mismatch unrepresentable.
type AnthropicAdapter struct {
	BaseURL    string
	APIVersion string
	Extra      map[string]string

	client *Client
}

// NewAnthropicAdapter builds an Anthropic Messages adapter.
func NewAnthropicAdapter(baseURL string, extra map[string]string, client *Client) *AnthropicAdapter {
	if client == nil {
		client = NewClient()
	}
	if baseURL == "" {
		baseURL = AnthropicBaseURL
	}
	return &AnthropicAdapter{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIVersion: "2023-06-01",
		Extra:      extra,
		client:     client,
	}
}

func (a *AnthropicAdapter) Name() string { return "anthropic" }

func (a *AnthropicAdapter) Capabilities() Capability {
	// Anthropic has no embedding endpoint and does not expose STT or TTS
	// through the Messages API.
	return Capability{Images: true}
}

func (a *AnthropicAdapter) endpoint(req Request) string {
	if req.BaseURL != "" {
		return strings.TrimRight(req.BaseURL, "/")
	}
	return a.BaseURL
}

func (a *AnthropicAdapter) headers(apiKey string) map[string]string {
	h := map[string]string{
		"Content-Type":      "application/json",
		"x-api-key":         apiKey,
		"anthropic-version": a.APIVersion,
	}
	for k, v := range a.Extra {
		h[k] = v
	}
	return h
}

// --- request translation ---

// contentPart is one element of a multi-part message content array. It models
// both the OpenAI input shape and the Anthropic output shape, so a single type
// carries a block through translation in either direction.
type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// ImageURL is the OpenAI input form: a data URI or a remote URL. It must
	// be declared here rather than re-read from raw JSON, because a struct
	// that does not model the field will silently drop it on the first
	// unmarshal.
	ImageURL *imageURL `json:"image_url,omitempty"`
	// InputAudio carries base64 audio in the OpenAI shape.
	InputAudio *inputAudio `json:"input_audio,omitempty"`

	// Source is the Anthropic media block.
	Source *mediaSource `json:"source,omitempty"`
}

// imageURL wraps the URL of an OpenAI image part.
type imageURL struct {
	URL string `json:"url"`
}

// inputAudio wraps base64 audio in the OpenAI shape.
type inputAudio struct {
	Format string `json:"format"`
	Data   string `json:"data"`
}

type mediaSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	Messages  []anthropicMessage `json:"messages"`
	System    string             `json:"system,omitempty"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream,omitempty"`

	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	StopSeqs    []string `json:"stop_sequences,omitempty"`
}

// toAnthropic converts an OpenAI chat body into an Anthropic Messages request.
//
// System messages are hoisted to the top-level system field, image data URIs
// become base64 media blocks, and max_tokens is required by Anthropic where
// OpenAI treats it as optional, so a default is supplied.
func toAnthropic(body []byte, model string) (*anthropicRequest, error) {
	var src struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		MaxTokens   int      `json:"max_tokens"`
		Temperature *float64 `json:"temperature"`
		TopP        *float64 `json:"top_p"`
		Stop        any      `json:"stop"`
		Stream      bool     `json:"stream"`
	}
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	out := &anthropicRequest{
		Model:       stripProviderPrefix(model),
		MaxTokens:   4096,
		Temperature: src.Temperature,
		TopP:        src.TopP,
		Stream:      src.Stream,
	}
	if src.MaxTokens > 0 {
		out.MaxTokens = src.MaxTokens
	}
	switch stop := src.Stop.(type) {
	case string:
		out.StopSeqs = []string{stop}
	case []any:
		for _, s := range stop {
			if str, ok := s.(string); ok {
				out.StopSeqs = append(out.StopSeqs, str)
			}
		}
	}

	var systemParts []string
	for _, m := range src.Messages {
		if m.Role == "system" {
			systemParts = append(systemParts, flattenText(m.Content))
			continue
		}
		converted := convertContent(m.Content)
		out.Messages = append(out.Messages, anthropicMessage{Role: m.Role, Content: converted})
	}
	out.System = strings.Join(systemParts, "\n")
	return out, nil
}

// flattenText renders either a string or a multi-part array as plain text.
func flattenText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// convertContent maps OpenAI message content onto Anthropic content blocks.
// Plain strings stay strings; arrays become typed blocks.
func convertContent(raw json.RawMessage) any {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}

	out := make([]contentPart, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, contentPart{Type: "text", Text: p.Text})
		case "image_url":
			// OpenAI carries images as a URL that is usually a data URI;
			// Anthropic requires the base64 payload split out.
			if p.ImageURL == nil {
				continue
			}
			mediaType, data, ok := splitDataURI(p.ImageURL.URL)
			if !ok {
				// A remote URL has no base64 payload to forward. Anthropic
				// cannot fetch it, so it is dropped rather than sent as
				// broken content.
				continue
			}
			out = append(out, contentPart{
				Type:   "image",
				Source: &mediaSource{Type: "base64", MediaType: mediaType, Data: data},
			})
		case "input_audio":
			// Anthropic has no audio content block; base64 audio is dropped
			// rather than mislabelled as an image.
			continue
		}
	}
	return out
}

// splitDataURI parses "data:<media-type>;base64,<payload>".
func splitDataURI(uri string) (mediaType, data string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(uri, prefix) {
		return "", "", false
	}
	rest := uri[len(prefix):]
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", "", false
	}
	meta, payload := rest[:comma], rest[comma+1:]
	if !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}
	mediaType = strings.TrimSuffix(meta, ";base64")
	if mediaType == "" {
		return "", "", false
	}
	return mediaType, payload, true
}

// --- response translation ---

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicResponse struct {
	ID         string `json:"id"`
	Role       string `json:"role"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage anthropicUsage `json:"usage"`
}

// toOpenAI renders an Anthropic message as an OpenAI chat completion.
func (a *anthropicResponse) toOpenAI(model string) map[string]any {
	var content strings.Builder
	for _, block := range a.Content {
		if block.Type == "text" {
			content.WriteString(block.Text)
		}
	}
	return map[string]any{
		"id":      firstNonEmpty(a.ID, "msg_"+newID()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": roleOr(a.Role, "assistant"), "content": content.String()},
			"finish_reason": mapStopReason(a.StopReason),
		}},
		"usage": map[string]any{
			"prompt_tokens":     a.Usage.InputTokens,
			"completion_tokens": a.Usage.OutputTokens,
			"total_tokens":      a.Usage.InputTokens + a.Usage.OutputTokens,
		},
	}
}

func roleOr(role, def string) string {
	if role == "" {
		return def
	}
	return role
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// mapStopReason translates Anthropic stop reasons onto OpenAI finish reasons.
func mapStopReason(reason string) string {
	switch reason {
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	default:
		// "end_turn" and anything unrecognised are ordinary completions.
		return "stop"
	}
}

// --- Adapter implementation ---

func (a *AnthropicAdapter) Complete(ctx context.Context, req Request) (*Response, error) {
	payload, err := toAnthropic(req.Body, req.Model)
	if err != nil {
		return nil, err
	}
	payload.Stream = false
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	httpReq, err := a.client.BuildRequest(ctx, "POST", joinURL(a.endpoint(req), "messages"), strings.NewReader(string(encoded)), a.headers(req.APIKey))
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &EndpointError{StatusCode: resp.StatusCode, Body: raw, Provider: "anthropic"}
	}

	var decoded anthropicResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("providers: decode anthropic response: %w", err)
	}
	body, err := json.Marshal(decoded.toOpenAI(stripProviderPrefix(req.Model)))
	if err != nil {
		return nil, err
	}
	return &Response{
		StatusCode:  resp.StatusCode,
		Header:      resp.Header,
		Body:        body,
		ContentType: "application/json",
	}, nil
}

// Stream converts Anthropic's typed SSE events into OpenAI delta chunks as
// they arrive, so the client sees a normal chat.completion.chunk stream.
func (a *AnthropicAdapter) Stream(ctx context.Context, req Request) (Stream, error) {
	payload, err := toAnthropic(req.Body, req.Model)
	if err != nil {
		return nil, err
	}
	payload.Stream = true
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	headers := a.headers(req.APIKey)
	headers["Accept"] = "text/event-stream"

	httpReq, err := a.client.BuildRequest(ctx, "POST", joinURL(a.endpoint(req), "messages"), strings.NewReader(string(encoded)), headers)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return nil, &EndpointError{StatusCode: resp.StatusCode, Body: raw, Provider: "anthropic"}
	}
	return &anthropicStream{inner: newSSEStream(resp.Body), model: stripProviderPrefix(req.Model)}, nil
}

// anthropicStream rewrites Anthropic's event protocol into OpenAI chunks.
type anthropicStream struct {
	inner *sseStream
	model string

	eventType string
	msgID     string
	// usage accumulates across message_start and message_delta, because
	// Anthropic reports input tokens at the start and output tokens at the end.
	promptTokens     int
	completionTokens int
}

func (s *anthropicStream) Next() (*Chunk, error) {
	for {
		chunk, err := s.inner.Next()
		if err != nil {
			return nil, err
		}
		if chunk.Done {
			return &Chunk{Done: true}, nil
		}

		// Anthropic frames carry the event name in a separate "event:" line,
		// so the type has to be tracked across reads.
		text := string(chunk.Data)
		if strings.HasPrefix(text, "[DONE]") {
			return &Chunk{Done: true}, nil
		}

		var envelope struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
			Message struct {
				ID    string         `json:"id"`
				Usage anthropicUsage `json:"usage"`
			} `json:"message"`
			Usage anthropicUsage `json:"usage"`
		}
		if err := json.Unmarshal(chunk.Data, &envelope); err != nil {
			continue
		}

		event := envelope.Type
		if event == "" {
			event = s.eventType
		}
		s.eventType = event

		switch event {
		case "message_start":
			s.msgID = envelope.Message.ID
			s.promptTokens = envelope.Message.Usage.InputTokens
			// A role-only chunk opens the stream for clients that expect one.
			return s.emit(map[string]any{
				"id": s.msgID, "model": s.model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
			}), nil

		case "content_block_delta":
			if envelope.Delta.Type != "text_delta" || envelope.Delta.Text == "" {
				continue
			}
			return s.emit(map[string]any{
				"id": s.msgID, "model": s.model,
				"choices": []map[string]any{{
					"index":         envelope.Index,
					"delta":         map[string]any{"content": envelope.Delta.Text},
					"finish_reason": nil,
				}},
			}), nil

		case "message_delta":
			// The final usage report arrives here.
			s.completionTokens = envelope.Usage.OutputTokens
			final := &Chunk{}
			if envelope.Usage.OutputTokens > 0 || s.promptTokens > 0 {
				final.Usage = &Usage{
					PromptTokens:     s.promptTokens,
					CompletionTokens: s.completionTokens,
					TotalTokens:      s.promptTokens + s.completionTokens,
				}
			}
			return final, nil

		case "message_stop":
			return &Chunk{Done: true}, nil
		}
	}
}

// emit marshals one OpenAI chunk payload.
func (s *anthropicStream) emit(v map[string]any) *Chunk {
	v["object"] = "chat.completion.chunk"
	v["created"] = time.Now().Unix()
	data, err := json.Marshal(v)
	if err != nil {
		return &Chunk{}
	}
	return &Chunk{Data: data}
}

func (s *anthropicStream) Close() error { return s.inner.Close() }

// Embeddings is unavailable: Anthropic exposes no embedding endpoint.
func (a *AnthropicAdapter) Embeddings(ctx context.Context, req Request) (*Response, error) {
	return nil, fmt.Errorf("%w: anthropic has no embeddings endpoint", ErrUnsupported)
}

// Transcribe is unavailable through the Messages API.
func (a *AnthropicAdapter) Transcribe(ctx context.Context, req Request, audio AudioFile) (*Response, error) {
	return nil, fmt.Errorf("%w: anthropic has no transcription endpoint", ErrUnsupported)
}

// Speak is unavailable through the Messages API.
func (a *AnthropicAdapter) Speak(ctx context.Context, req Request) (*Response, error) {
	return nil, fmt.Errorf("%w: anthropic has no speech endpoint", ErrUnsupported)
}

// Probe infers capabilities from the error text of a deliberately malformed
// request, since Anthropic rejects unknown fields loudly.
func (a *AnthropicAdapter) Probe(ctx context.Context, req Request) (ModelCapability, error) {
	caps := ModelCapability{SupportsStream: true, SupportsVision: true}
	payload, err := toAnthropic(req.Body, req.Model)
	if err != nil {
		return caps, nil
	}
	payload.MaxTokens = 1
	encoded, _ := json.Marshal(payload)

	httpReq, err := a.client.BuildRequest(ctx, "POST", joinURL(a.endpoint(req), "messages"), strings.NewReader(string(encoded)), a.headers(req.APIKey))
	if err != nil {
		return caps, nil
	}
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return caps, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		caps.SupportsTools = true
	}
	return caps, nil
}

var _ Adapter = (*AnthropicAdapter)(nil)
