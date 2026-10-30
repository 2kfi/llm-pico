package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

// GeminiBaseURL is the upstream used when a model configures no api_base.
const GeminiBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// GeminiAdapter translates between the OpenAI chat format and Google's
// generateContent API.
//
// Google's protocol differs in three ways that matter: the model name is part
// of the URL path, the API key is a query parameter, and the response envelope
// holds candidates rather than choices.
type GeminiAdapter struct {
	BaseURL    string
	QueryParam string
	Extra      map[string]string

	client *Client
}

// NewGeminiAdapter builds a Google Generative Language adapter.
func NewGeminiAdapter(baseURL string, extra map[string]string, client *Client) *GeminiAdapter {
	if client == nil {
		client = NewClient()
	}
	if baseURL == "" {
		baseURL = GeminiBaseURL
	}
	return &GeminiAdapter{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		QueryParam: "key",
		Extra:      extra,
		client:     client,
	}
}

func (a *GeminiAdapter) Name() string { return "gemini" }

func (a *GeminiAdapter) Capabilities() Capability {
	return Capability{Images: true, Embeddings: true}
}

func (a *GeminiAdapter) endpoint(req Request) string {
	base := a.BaseURL
	if req.BaseURL != "" {
		base = strings.TrimRight(req.BaseURL, "/")
	}
	return base
}

// withKey appends the API key as an escaped query parameter.
func (a *GeminiAdapter) withKey(rawURL, apiKey string) string {
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	param := a.QueryParam
	if param == "" {
		param = "key"
	}
	return rawURL + sep + param + "=" + url.QueryEscape(apiKey)
}

func (a *GeminiAdapter) headers() map[string]string {
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range a.Extra {
		h[k] = v
	}
	return h
}

type geminiPart struct {
	Text       string       `json:"text,omitempty"`
	InlineData *inlineMedia `json:"inlineData,omitempty"`
}

type inlineMedia struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiRequest struct {
	Contents          []geminiContent `json:"contents"`
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	GenerationConfig  *generationCfg  `json:"generationConfig,omitempty"`
}

type generationCfg struct {
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

// toGemini converts an OpenAI chat body into a generateContent request.
func toGemini(body []byte) (*geminiRequest, error) {
	var src struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		MaxTokens   int      `json:"max_tokens"`
		Temperature *float64 `json:"temperature"`
		TopP        *float64 `json:"top_p"`
		Stop        any      `json:"stop"`
	}
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	out := &geminiRequest{
		GenerationConfig: &generationCfg{
			MaxOutputTokens: src.MaxTokens,
			Temperature:     src.Temperature,
			TopP:            src.TopP,
		},
	}
	switch stop := src.Stop.(type) {
	case string:
		out.GenerationConfig.StopSequences = []string{stop}
	case []any:
		for _, s := range stop {
			if str, ok := s.(string); ok {
				out.GenerationConfig.StopSequences = append(out.GenerationConfig.StopSequences, str)
			}
		}
	}
	if out.GenerationConfig.MaxOutputTokens <= 0 {
		out.GenerationConfig.MaxOutputTokens = 4096
	}

	for _, m := range src.Messages {
		if m.Role == "system" {
			// Gemini carries the system prompt out of band from the turn list.
			out.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: flattenText(m.Content)}}}
			continue
		}
		// Gemini names roles "user" and "model"; "assistant" maps to "model".
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		out.Contents = append(out.Contents, geminiContent{Role: role, Parts: convertGeminiParts(m.Content)})
	}
	return out, nil
}

// convertGeminiParts maps OpenAI content onto Gemini parts, including base64
// inline media carried in data URIs.
func convertGeminiParts(raw json.RawMessage) []geminiPart {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []geminiPart{{Text: s}}
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]geminiPart, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, geminiPart{Text: p.Text})
		case "image_url":
			if p.ImageURL == nil {
				continue
			}
			mediaType, data, ok := splitDataURI(p.ImageURL.URL)
			if !ok {
				continue
			}
			out = append(out, geminiPart{InlineData: &inlineMedia{MimeType: mediaType, Data: data}})
		}
	}
	return out
}

type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// toOpenAI renders a generateContent reply as an OpenAI chat completion.
// Only the first candidate is surfaced: OpenAI's default shape is one choice.
func (g *geminiResponse) toOpenAI(model string) map[string]any {
	var content strings.Builder
	var finish string
	if len(g.Candidates) > 0 {
		finish = g.Candidates[0].FinishReason
		for _, p := range g.Candidates[0].Content.Parts {
			content.WriteString(p.Text)
		}
	}
	total := g.UsageMetadata.TotalTokenCount
	if total == 0 {
		total = g.UsageMetadata.PromptTokenCount + g.UsageMetadata.CandidatesTokenCount
	}
	return map[string]any{
		"id":      newID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content.String()},
			"finish_reason": mapGeminiFinish(finish),
		}},
		"usage": map[string]any{
			"prompt_tokens":     g.UsageMetadata.PromptTokenCount,
			"completion_tokens": g.UsageMetadata.CandidatesTokenCount,
			"total_tokens":      total,
		},
	}
}

func mapGeminiFinish(reason string) string {
	switch reason {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "PROHIBITED_CONTENT":
		return "content_filter"
	default:
		return "stop"
	}
}

func (a *GeminiAdapter) Complete(ctx context.Context, req Request) (*Response, error) {
	payload, err := toGemini(req.Body)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	model := stripProviderPrefix(req.Model)
	url := a.withKey(joinURL(a.endpoint(req), "models/"+model+":generateContent"), req.APIKey)

	httpReq, err := a.client.BuildRequest(ctx, "POST", url, strings.NewReader(string(encoded)), a.headers())
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
		return nil, &EndpointError{StatusCode: resp.StatusCode, Body: raw, Provider: "gemini"}
	}

	var decoded geminiResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("providers: decode gemini response: %w", err)
	}
	body, err := json.Marshal(decoded.toOpenAI(model))
	if err != nil {
		return nil, err
	}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: body, ContentType: "application/json"}, nil
}

// Stream converts Gemini's streamed generateContent chunks into OpenAI deltas.
func (a *GeminiAdapter) Stream(ctx context.Context, req Request) (Stream, error) {
	payload, err := toGemini(req.Body)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	model := stripProviderPrefix(req.Model)
	// alt=sse switches Google's stream endpoint from its default chunked
	// JSON array to the server-sent-event framing the SSE reader expects.
	streamURL := joinURL(a.endpoint(req), "models/"+model+":streamGenerateContent")
	streamURL += "?alt=sse"
	url := a.withKey(streamURL, req.APIKey)

	headers := a.headers()
	headers["Accept"] = "text/event-stream"

	httpReq, err := a.client.BuildRequest(ctx, "POST", url, strings.NewReader(string(encoded)), headers)
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
		return nil, &EndpointError{StatusCode: resp.StatusCode, Body: raw, Provider: "gemini"}
	}
	return &geminiStream{inner: newSSEStream(resp.Body), model: model}, nil
}

type geminiStream struct {
	inner *sseStream
	model string
}

// geminiChunkText extracts candidate 0's text and finish reason from one
// streamed generateContent payload.
func geminiChunkText(decoded geminiResponse) (string, string) {
	var text strings.Builder
	var finish string
	if len(decoded.Candidates) > 0 {
		finish = decoded.Candidates[0].FinishReason
		for _, p := range decoded.Candidates[0].Content.Parts {
			text.WriteString(p.Text)
		}
	}
	return text.String(), finish
}

// openAIChunkPayload builds an OpenAI chat.completion.chunk body out of a
// text delta and/or a finish reason, mapping Gemini's finish vocabulary.
func streamChunkPayload(model string, text string, finish string) ([]byte, bool) {
	if text == "" && finish == "" {
		return nil, false
	}
	choice := map[string]any{
		"index":         0,
		"delta":         map[string]any{},
		"finish_reason": nil,
	}
	if text != "" {
		choice["delta"] = map[string]any{"content": text}
	}
	if finish != "" {
		choice["finish_reason"] = mapGeminiFinish(finish)
	}
	payload := map[string]any{
		"id":      newID(),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{choice},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func (s *geminiStream) Next() (*Chunk, error) {
	for {
		chunk, err := s.inner.Next()
		if err != nil {
			return nil, err
		}
		if chunk.Done {
			return &Chunk{Done: true}, nil
		}

		var decoded geminiResponse
		if err := json.Unmarshal(chunk.Data, &decoded); err != nil {
			continue
		}

		out := &Chunk{}
		text, finish := geminiChunkText(decoded)
		out.Usage = nil
		if decoded.UsageMetadata.TotalTokenCount > 0 {
			total := decoded.UsageMetadata.TotalTokenCount
			if total == 0 {
				total = decoded.UsageMetadata.PromptTokenCount + decoded.UsageMetadata.CandidatesTokenCount
			}
			out.Usage = &Usage{
				PromptTokens:     decoded.UsageMetadata.PromptTokenCount,
				CompletionTokens: decoded.UsageMetadata.CandidatesTokenCount,
				TotalTokens:      total,
			}
		}
		// Forward every finish reason, including STOP, so clients see a
		// complete OpenAI-style termination signal before [DONE].
		if data, ok := streamChunkPayload(s.model, text, finish); ok {
			out.Data = data
		}
		if out.Data != nil || out.Usage != nil {
			return out, nil
		}
	}
}

func (s *geminiStream) Close() error { return s.inner.Close() }

// Embeddings maps onto Google's embedContent endpoint, counting tokens locally
// because the OpenAI shape has no usage block.
func (a *GeminiAdapter) Embeddings(ctx context.Context, req Request) (*Response, error) {
	fields, err := peekFields(req.Body)
	if err != nil {
		return nil, err
	}
	model := stripProviderPrefix(req.Model)
	// The body's model field is the caller-facing name; routing already
	// resolved the upstream model, and the body name must never override it.

	input := fields["input"]
	parts := convertEmbedInput(input)
	if len(parts) == 0 {
		return nil, fmt.Errorf("%w: embeddings require a non-empty input", ErrInvalidRequest)
	}

	payload := map[string]any{
		"requests": []map[string]any{
			{"model": "models/" + model, "content": geminiContent{Parts: parts}},
		},
	}
	encoded, _ := json.Marshal(payload)
	url := a.withKey(joinURL(a.endpoint(req), "models/"+model+":batchEmbedContents"), req.APIKey)

	httpReq, err := a.client.BuildRequest(ctx, "POST", url, strings.NewReader(string(encoded)), a.headers())
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
		return nil, &EndpointError{StatusCode: resp.StatusCode, Body: raw, Provider: "gemini"}
	}

	var decoded struct {
		Embeddings []struct {
			Values []float64 `json:"values"`
		} `json:"embeddings"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("providers: decode gemini embeddings: %w", err)
	}

	data := make([]map[string]any, 0, len(decoded.Embeddings))
	for i, e := range decoded.Embeddings {
		data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": e.Values})
	}
	// The estimate covers the whole input once: multiplying it per returned
	// embedding double-counted the request.
	tokens := estimateTokens(parts)
	body, _ := json.Marshal(map[string]any{
		"object": "list",
		"data":   data,
		"model":  model,
		"usage":  map[string]any{"prompt_tokens": tokens, "total_tokens": tokens},
	})
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: body, ContentType: "application/json"}, nil
}

// convertEmbedInput normalizes the several shapes the OpenAI input field
// accepts into Gemini content parts.
func convertEmbedInput(raw json.RawMessage) []geminiPart {
	if len(raw) == 0 {
		return nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []geminiPart{{Text: single}}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		parts := make([]geminiPart, 0, len(many))
		for _, s := range many {
			parts = append(parts, geminiPart{Text: s})
		}
		return parts
	}
	// Token-array inputs cannot be rendered as text.
	return nil
}

// estimateTokens approximates a token count for embedding usage reporting.
// Exact counting would require the provider's tokenizer; this is only used
// where the provider reports no counts at all.
func estimateTokens(parts []geminiPart) int {
	total := 0
	for _, p := range parts {
		total += len(p.Text)/4 + 1
	}
	return total
}

// Transcribe is unavailable: speech-to-text needs the separate Media API.
func (a *GeminiAdapter) Transcribe(ctx context.Context, req Request, audio AudioFile) (*Response, error) {
	return nil, fmt.Errorf("%w: gemini transcription requires the Media API", ErrUnsupported)
}

// Speak is unavailable through generateContent.
func (a *GeminiAdapter) Speak(ctx context.Context, req Request) (*Response, error) {
	return nil, fmt.Errorf("%w: gemini has no speech endpoint", ErrUnsupported)
}

func (a *GeminiAdapter) Probe(ctx context.Context, req Request) (ModelCapability, error) {
	// Gemini models all support vision and JSON-style responses; probing would
	// cost a real generation for no additional signal.
	return ModelCapability{SupportsVision: true, SupportsStream: true, SupportsJSON: true}, nil
}

var _ Adapter = (*GeminiAdapter)(nil)
