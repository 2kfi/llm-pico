package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// AnthropicBaseURL is the upstream used when a model configures no api_base.
const AnthropicBaseURL = "https://api.anthropic.com/v1"

// anthropicDefaultMaxTokens is the completion cap used when the request sets
// none. Anthropic requires the field; the old 4096 default silently truncated
// agent replies, so the default leaves room for a full tool turn.
const anthropicDefaultMaxTokens = 16384

// AnthropicAdapter translates between the OpenAI chat format and Anthropic's
// Messages API.
//
// Its streaming protocol is a typed event sequence rather than OpenAI's delta
// chunks. The adapter converts events into chat.completion.chunk frames as
// they arrive, so the proxy's streaming path is identical for every provider.
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
	// through the Messages API. Tools, extended thinking and prompt caching
	// are all part of it.
	return Capability{Images: true, Tools: true, Reasoning: true, Cache: true}
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

// contentPart is one element of a multi-part message content array. It models
// the OpenAI input shape, the Anthropic block shape and media blocks, so a
// single type carries a block through translation in either direction.
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

	// ID, Name and Input are the tool_use block.
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// ToolUseID and Content are the tool_result block.
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`

	// CacheControl marks a prompt-cache breakpoint.
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"`
}

// imageURL wraps the URL of an OpenAI image part. The chat API wraps it in an
// object while the responses shape sends a bare string; both decode here,
// because a field that fails to decode takes the whole message's content down
// with it and silently drops the request's text too.
type imageURL struct {
	URL string `json:"url"`
}

func (u *imageURL) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		u.URL = s
		return nil
	}
	var obj struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		// An unreadable URL leaves the part empty; the converters skip it
		// instead of failing every sibling part.
		return nil
	}
	u.URL = obj.URL
	return nil
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
	System    any                `json:"system,omitempty"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream,omitempty"`

	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	StopSeqs    []string `json:"stop_sequences,omitempty"`

	Tools      []anthropicTool      `json:"tools,omitempty"`
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
	Thinking   *anthropicThinking   `json:"thinking,omitempty"`
}

// anthropicTool forwards the caller's JSON Schema untouched through
// input_schema: the Messages API accepts full JSON Schema there.
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// anthropicConfig carries per-model knobs into the translation. It is separate
// from the request body because these are operator settings, not caller ones.
type anthropicConfig struct {
	// MaxOutput is the completion cap to use when the caller sets none.
	MaxOutput int
	// NoCache disables the default prompt-cache breakpoints.
	NoCache bool
}

// Reasoning-effort presets, expressed as Anthropic thinking token budgets.
// Anthropic requires at least 1024.
const (
	anthropicEffortLow    = 1024
	anthropicEffortMedium = 8192
	anthropicEffortHigh   = 16384
)

// anthropicEffortBudget maps OpenAI's reasoning_effort vocabulary onto a
// thinking budget. ok is false when the effort is not a known preset.
func anthropicEffortBudget(effort string) (int, bool) {
	switch effort {
	case "none":
		return 0, true
	case "low":
		return anthropicEffortLow, true
	case "medium":
		return anthropicEffortMedium, true
	case "high":
		return anthropicEffortHigh, true
	default:
		return 0, false
	}
}

// toAnthropic converts an OpenAI chat body into an Anthropic Messages request
// with the adapter's default knobs.
func toAnthropic(body []byte, model string) (*anthropicRequest, error) {
	return buildAnthropic(body, model, anthropicConfig{})
}

// buildAnthropic converts an OpenAI chat body into an Anthropic Messages
// request.
//
// System messages are hoisted to the top-level system field, image data URIs
// become base64 media blocks, assistant tool calls become tool_use blocks and
// tool results become tool_result blocks in a user turn. Anthropic requires
// max_tokens, so a default (or the model's max_output) is supplied.
func buildAnthropic(body []byte, model string, cfg anthropicConfig) (*anthropicRequest, error) {
	var src struct {
		Messages            []chatMessage `json:"messages"`
		MaxTokens           int           `json:"max_tokens"`
		MaxCompletionTokens int           `json:"max_completion_tokens"`
		Temperature         *float64      `json:"temperature"`
		TopP                *float64      `json:"top_p"`
		Stop                any           `json:"stop"`
		Stream              bool          `json:"stream"`
		Tools               []chatTool    `json:"tools"`
		ToolChoice          any           `json:"tool_choice"`
		ReasoningEffort     string        `json:"reasoning_effort"`
		ThinkingBudget      *int          `json:"thinking_budget"`
	}
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	out := &anthropicRequest{
		Model:       stripProviderPrefix(model),
		MaxTokens:   anthropicDefaultMaxTokens,
		Temperature: src.Temperature,
		TopP:        src.TopP,
		Stream:      src.Stream,
	}
	if cfg.MaxOutput > 0 {
		out.MaxTokens = cfg.MaxOutput
	}
	// The newer OpenAI field wins when both are sent, mirroring the OpenAI
	// adapter's precedence; ignoring it silently fell back to the default.
	if src.MaxCompletionTokens > 0 {
		out.MaxTokens = src.MaxCompletionTokens
	} else if src.MaxTokens > 0 {
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

	for _, t := range src.Tools {
		// Non-function tools have no Messages API equivalent and are skipped
		// rather than sent as a malformed declaration.
		if t.Type != "" && t.Type != "function" {
			continue
		}
		if t.Function.Name == "" {
			continue
		}
		schema := t.Function.Parameters
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	if len(out.Tools) > 0 {
		out.ToolChoice = anthropicToolChoiceFrom(src.ToolChoice)
	}

	// Tool call ids are opaque to Anthropic but must round-trip exactly:
	// every tool_result is paired by id, so resolve ids by function name for
	// callers that omit tool_call_id on the result.
	idsByName := map[string]string{}
	for _, m := range src.Messages {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && tc.Function.Name != "" {
				idsByName[tc.Function.Name] = tc.ID
			}
		}
	}

	var system []contentPart
	for _, m := range src.Messages {
		switch m.Role {
		case "system", "developer":
			if text := flattenText(m.Content); text != "" {
				system = append(system, contentPart{Type: "text", Text: text})
			}
			continue
		}

		role := "user"
		var parts []contentPart
		switch m.Role {
		case "assistant":
			role = "assistant"
			// An assistant turn that only called tools carries null content;
			// emitting a blank text block would be rejected.
			parts = convertContentParts(m.Content)
			for _, tc := range m.ToolCalls {
				id := tc.ID
				if id == "" {
					id = "toolu_" + newID()
				}
				parts = append(parts, contentPart{
					Type:  "tool_use",
					ID:    id,
					Name:  tc.Function.Name,
					Input: toolArguments(tc.Function.Arguments),
				})
			}
		case "tool", "function":
			id := firstNonEmpty(m.ToolCallID, idsByName[m.Name])
			if id == "" {
				// An id-less tool result cannot be paired, and Anthropic
				// rejects it rather than guessing; dropping it keeps the rest
				// of the conversation usable.
				continue
			}
			parts = []contentPart{{
				Type:      "tool_result",
				ToolUseID: id,
				Content:   jsonString(flattenText(m.Content)),
			}}
		default:
			parts = convertContentParts(m.Content)
		}
		if len(parts) == 0 {
			continue
		}
		// Anthropic expects alternating roles, so adjacent same-role turns
		// (parallel tool results, for example) become one message.
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
			if prev, ok := out.Messages[n-1].Content.([]contentPart); ok {
				out.Messages[n-1].Content = append(prev, parts...)
				continue
			}
		}
		out.Messages = append(out.Messages, anthropicMessage{Role: role, Content: parts})
	}
	if len(system) > 0 {
		out.System = system
	}

	// An explicit thinking budget wins over the preset, including 0 (disable
	// thinking), because presence of the field is what matters.
	budget := 0
	if src.ThinkingBudget != nil {
		budget = *src.ThinkingBudget
	} else if b, ok := anthropicEffortBudget(src.ReasoningEffort); ok {
		budget = b
	}
	if budget > 0 {
		out.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget}
		// Anthropic requires max_tokens to exceed the thinking budget, and
		// rejects a custom temperature alongside thinking.
		if out.MaxTokens <= budget {
			out.MaxTokens = budget + 4096
		}
		out.Temperature = nil
	}

	if !cfg.NoCache {
		out.injectCacheBreakpoints()
	}
	return out, nil
}

// anthropicToolChoiceFrom maps OpenAI's tool_choice onto Anthropic's. Auto is
// Anthropic's default, so it needs no field at all.
func anthropicToolChoiceFrom(choice any) *anthropicToolChoice {
	switch v := choice.(type) {
	case string:
		switch v {
		case "none":
			return &anthropicToolChoice{Type: "none"}
		case "required", "any":
			return &anthropicToolChoice{Type: "any"}
		}
	case map[string]any:
		if v["type"] != "function" {
			return nil
		}
		fn, _ := v["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			return nil
		}
		return &anthropicToolChoice{Type: "tool", Name: name}
	}
	return nil
}

// injectCacheBreakpoints marks the end of the stable prefix — the system
// prompt and the first conversation message — so repeated agent turns read it
// from cache at a fraction of the input price. A caller that already set its
// own breakpoint is left alone.
func (r *anthropicRequest) injectCacheBreakpoints() {
	// Anthropic accepts at most four breakpoints per request; a caller that
	// already used them all must not be pushed over the limit.
	used := r.cacheBreakpointCount()
	if used >= anthropicMaxCacheBreakpoints {
		return
	}
	if system, ok := r.System.([]contentPart); ok && len(system) > 0 {
		if system[len(system)-1].CacheControl == nil {
			system[len(system)-1].CacheControl = &cacheControl{Type: "ephemeral"}
			used++
		}
		r.System = system
	}
	if used >= anthropicMaxCacheBreakpoints {
		return
	}
	for i := range r.Messages {
		parts, ok := r.Messages[i].Content.([]contentPart)
		if !ok || len(parts) == 0 {
			continue
		}
		if parts[len(parts)-1].CacheControl == nil {
			parts[len(parts)-1].CacheControl = &cacheControl{Type: "ephemeral"}
		}
		r.Messages[i].Content = parts
		return
	}
}

// anthropicMaxCacheBreakpoints is the API ceiling on cache_control blocks.
const anthropicMaxCacheBreakpoints = 4

// cacheBreakpointCount reports how many breakpoints the caller already set.
func (r *anthropicRequest) cacheBreakpointCount() int {
	n := 0
	if system, ok := r.System.([]contentPart); ok {
		for _, p := range system {
			if p.CacheControl != nil {
				n++
			}
		}
	}
	for _, m := range r.Messages {
		if parts, ok := m.Content.([]contentPart); ok {
			for _, p := range parts {
				if p.CacheControl != nil {
					n++
				}
			}
		}
	}
	return n
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

// jsonString renders s as a JSON string value.
func jsonString(s string) json.RawMessage {
	encoded, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return encoded
}

// convertContentParts maps OpenAI message content onto Anthropic content
// blocks. Plain strings become one text block; arrays become typed blocks.
func convertContentParts(raw json.RawMessage) []contentPart {
	// An assistant turn that only called tools carries a null or empty
	// content; emitting a blank text block would be rejected.
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []contentPart{{Type: "text", Text: s}}
	}

	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}

	out := make([]contentPart, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, contentPart{Type: "text", Text: p.Text})
		case "image_url", "input_image":
			// OpenAI carries images as a URL that is usually a data URI;
			// Anthropic requires the base64 payload split out. Both part
			// names carry the same field.
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

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// promptTotal is the full input size: Anthropic reports cached reads and
// cache writes separately from newly processed input.
func (u anthropicUsage) promptTotal() int {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

type anthropicResponse struct {
	ID         string `json:"id"`
	Role       string `json:"role"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	Usage anthropicUsage `json:"usage"`
}

// toOpenAI renders an Anthropic message as an OpenAI chat completion.
func (a *anthropicResponse) toOpenAI(model string) map[string]any {
	var content strings.Builder
	var calls []map[string]any
	for _, block := range a.Content {
		switch block.Type {
		case "text":
			content.WriteString(block.Text)
		case "tool_use":
			args := "{}"
			if len(block.Input) > 0 && string(block.Input) != "null" {
				args = string(block.Input)
			}
			calls = append(calls, map[string]any{
				"id":   firstNonEmpty(block.ID, "call_"+newID()),
				"type": "function",
				"function": map[string]any{
					"name":      block.Name,
					"arguments": args,
				},
			})
		}
	}

	message := map[string]any{"role": roleOr(a.Role, "assistant"), "content": content.String()}
	reason := mapStopReason(a.StopReason)
	if len(calls) > 0 {
		message["tool_calls"] = calls
		if content.Len() == 0 {
			message["content"] = nil
		}
		if reason == "stop" {
			reason = "tool_calls"
		}
	}

	prompt := a.Usage.promptTotal()
	usage := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": a.Usage.OutputTokens,
		"total_tokens":      prompt + a.Usage.OutputTokens,
	}
	if a.Usage.CacheReadInputTokens > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": a.Usage.CacheReadInputTokens}
	}

	return map[string]any{
		"id":      firstNonEmpty(a.ID, "msg_"+newID()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": reason,
		}},
		"usage": usage,
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

func (a *AnthropicAdapter) Complete(ctx context.Context, req Request) (*Response, error) {
	payload, err := buildAnthropic(req.Body, req.Model, anthropicConfig{
		MaxOutput: req.MaxOutput,
		NoCache:   req.NoCache,
	})
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

	raw, err := readAllLimited(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, endpointError(resp, raw, "anthropic")
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
	payload, err := buildAnthropic(req.Body, req.Model, anthropicConfig{
		MaxOutput: req.MaxOutput,
		NoCache:   req.NoCache,
	})
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
		raw, _ := readAllLimited(resp.Body)
		return nil, endpointError(resp, raw, "anthropic")
	}
	return &anthropicStream{
		inner:            newSSEStream(resp.Body),
		model:            stripProviderPrefix(req.Model),
		toolIndexByBlock: map[int]int{},
	}, nil
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
	cachedTokens     int
	completionTokens int
	stopReason       string

	// toolIndexByBlock maps an Anthropic content-block index onto the
	// message-wide tool call index OpenAI clients accumulate by.
	toolIndexByBlock map[int]int
	toolCalls        int
	sawToolCalls     bool
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
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
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
			s.promptTokens = envelope.Message.Usage.promptTotal()
			s.cachedTokens = envelope.Message.Usage.CacheReadInputTokens
			// A role-only chunk opens the stream for clients that expect one.
			return s.emit(map[string]any{
				"id": s.msgID, "model": s.model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
			}), nil

		case "content_block_start":
			if envelope.ContentBlock.Type != "tool_use" {
				continue
			}
			index := s.toolCalls
			s.toolIndexByBlock[envelope.Index] = index
			s.toolCalls++
			s.sawToolCalls = true
			// OpenAI opens a tool call with its id and name, then streams the
			// arguments as string fragments.
			return s.toolDelta(index, envelope.ContentBlock.ID, envelope.ContentBlock.Name, ""), nil

		case "content_block_delta":
			switch envelope.Delta.Type {
			case "text_delta":
				if envelope.Delta.Text == "" {
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
			case "input_json_delta":
				index, ok := s.toolIndexByBlock[envelope.Index]
				if !ok {
					continue
				}
				return s.toolDelta(index, "", "", envelope.Delta.PartialJSON), nil
			}

		case "message_delta":
			// The final usage report arrives here, alongside the stop reason.
			s.completionTokens = envelope.Usage.OutputTokens
			if envelope.Delta.StopReason != "" {
				s.stopReason = envelope.Delta.StopReason
			}
			return s.finalChunk(), nil

		case "message_stop":
			return &Chunk{Done: true}, nil
		}
	}
}

// toolDelta emits one OpenAI tool-call delta frame.
func (s *anthropicStream) toolDelta(index int, id, name, args string) *Chunk {
	fn := map[string]any{"arguments": args}
	if name != "" {
		fn["name"] = name
	}
	call := map[string]any{
		"index":    index,
		"type":     "function",
		"function": fn,
	}
	if id != "" {
		call["id"] = id
	}
	return s.emit(map[string]any{
		"id": s.msgID, "model": s.model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{"tool_calls": []map[string]any{call}},
			"finish_reason": nil,
		}},
	})
}

// finalChunk carries the accumulated usage and, when one arrived, the
// translated finish reason.
func (s *anthropicStream) finalChunk() *Chunk {
	out := &Chunk{}
	if s.promptTokens > 0 || s.completionTokens > 0 {
		out.Usage = &Usage{
			PromptTokens:     s.promptTokens,
			CompletionTokens: s.completionTokens,
			TotalTokens:      s.promptTokens + s.completionTokens,
			CachedTokens:     s.cachedTokens,
		}
	}
	if s.stopReason == "" {
		return out
	}
	reason := mapStopReason(s.stopReason)
	if s.sawToolCalls && reason == "stop" {
		reason = "tool_calls"
	}
	out.Data = s.emitData(map[string]any{
		"id": s.msgID, "model": s.model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": reason}},
	})
	return out
}

// emit marshals one OpenAI chunk payload.
func (s *anthropicStream) emit(v map[string]any) *Chunk {
	return &Chunk{Data: s.emitData(v)}
}

func (s *anthropicStream) emitData(v map[string]any) []byte {
	v["object"] = "chat.completion.chunk"
	v["created"] = time.Now().Unix()
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
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
	// Tools are assumed until a provider rejects them explicitly: a probe
	// that fails for an unrelated reason must not hide agent support.
	caps := ModelCapability{SupportsStream: true, SupportsVision: true, SupportsTools: true}
	payload, err := buildAnthropic(req.Body, req.Model, anthropicConfig{})
	if err != nil {
		return caps, nil
	}
	payload.MaxTokens = 1
	payload.Thinking = nil
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
	body, _ := readAllLimited(resp.Body)
	// Only an explicit rejection clears tool support.
	if resp.StatusCode == http.StatusBadRequest {
		if strings.Contains(strings.ToLower(string(body)), "tool") {
			caps.SupportsTools = false
		}
	}
	return caps, nil
}

var _ Adapter = (*AnthropicAdapter)(nil)
