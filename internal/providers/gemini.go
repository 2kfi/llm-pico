package providers

import (
	"bytes"
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
	return Capability{Images: true, Audio: true, Video: true, Embeddings: true}
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
	Text             string                `json:"text,omitempty"`
	InlineData       *inlineMedia          `json:"inlineData,omitempty"`
	FunctionCall     *geminiFunctionCall   `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResult `json:"functionResponse,omitempty"`
	// Thought marks a reasoning summary; it is not part of the answer and
	// must not leak into the caller's content.
	Thought bool `json:"thought,omitempty"`
}

type inlineMedia struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// geminiFunctionCall is a model tool call; Args is a JSON object.
type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// geminiFunctionResult is a tool result being handed back to the model; it is
// keyed by function name rather than by call id.
type geminiFunctionResult struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiRequest struct {
	Contents          []geminiContent   `json:"contents"`
	SystemInstruction *geminiContent    `json:"systemInstruction,omitempty"`
	GenerationConfig  *generationCfg    `json:"generationConfig,omitempty"`
	Tools             []geminiTool      `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig `json:"toolConfig,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDecl `json:"functionDeclarations"`
}

// geminiFunctionDecl forwards the caller's JSON Schema untouched through
// parametersJsonSchema: the REST API accepts full JSON Schema there, while the
// older parameters field only accepts the OpenAPI 3.0 Schema subset.
type geminiFunctionDecl struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description,omitempty"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
}

type geminiToolConfig struct {
	FunctionCallingConfig geminiFunctionCallingConfig `json:"functionCallingConfig"`
}

type geminiFunctionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type generationCfg struct {
	MaxOutputTokens int          `json:"maxOutputTokens,omitempty"`
	Temperature     *float64     `json:"temperature,omitempty"`
	TopP            *float64     `json:"topP,omitempty"`
	StopSequences   []string     `json:"stopSequences,omitempty"`
	ThinkingConfig  *thinkingCfg `json:"thinkingConfig,omitempty"`
	// ResponseMimeType and ResponseJSONSchema implement OpenAI's
	// response_format: application/json plus an optional JSON Schema. The
	// JSON Schema is forwarded through responseJsonSchema, which accepts the
	// full vocabulary OpenAI clients send.
	ResponseMimeType   string          `json:"responseMimeType,omitempty"`
	ResponseJSONSchema json.RawMessage `json:"responseJsonSchema,omitempty"`
}

// thinkingCfg mirrors Gemini's thinking control inside generationConfig.
type thinkingCfg struct {
	// Budget is the thinking token allowance; 0 disables thinking (on
	// models that permit disabling it), -1 lets the model decide.
	Budget *int `json:"thinkingBudget,omitempty"`
}

// Reasoning-effort presets, expressed as thinking token budgets.
const (
	geminiEffortNone   = 0
	geminiEffortLow    = 1024
	geminiEffortMedium = 8192
	geminiEffortHigh   = 24576
)

// thinkingBudgetFrom maps the OpenAI reasoning_effort vocabulary onto a Gemini
// thinking token budget. ok is false when the effort is not a known preset.
func thinkingBudgetFrom(effort string) (int, bool) {
	switch effort {
	case "none":
		return geminiEffortNone, true
	case "low":
		return geminiEffortLow, true
	case "medium":
		return geminiEffortMedium, true
	case "high":
		return geminiEffortHigh, true
	default:
		return 0, false
	}
}

// chatMessage is one OpenAI chat turn, including the tool-call fields that
// have no direct Gemini equivalent.
type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatTool mirrors one OpenAI tool definition.
type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// chatResponseFormat mirrors OpenAI's response_format field.
type chatResponseFormat struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
	} `json:"json_schema"`
}

// toGemini converts an OpenAI chat body into a generateContent request.
func toGemini(body []byte) (*geminiRequest, error) {
	var src struct {
		Messages            []chatMessage       `json:"messages"`
		MaxTokens           int                 `json:"max_tokens"`
		MaxCompletionTokens int                 `json:"max_completion_tokens"`
		Temperature         *float64            `json:"temperature"`
		TopP                *float64            `json:"top_p"`
		Stop                any                 `json:"stop"`
		ReasoningEffort     string              `json:"reasoning_effort"`
		ThinkingBudget      *int                `json:"thinking_budget"`
		Tools               []chatTool          `json:"tools"`
		ToolChoice          any                 `json:"tool_choice"`
		ResponseFormat      *chatResponseFormat `json:"response_format"`
	}
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	// The newer OpenAI field wins when both are sent, mirroring the OpenAI
	// adapter's precedence.
	maxTokens := src.MaxTokens
	if src.MaxCompletionTokens > 0 {
		maxTokens = src.MaxCompletionTokens
	}
	out := &geminiRequest{
		GenerationConfig: &generationCfg{
			MaxOutputTokens: maxTokens,
			Temperature:     src.Temperature,
			TopP:            src.TopP,
		},
	}
	// An explicit thinking budget wins over the preset, including 0 (disable
	// thinking) and -1 (let the model decide). Presence of the field is what
	// matters, so it cannot be modelled as a plain int: absent and 0 differ.
	if src.ThinkingBudget != nil {
		budget := *src.ThinkingBudget
		out.GenerationConfig.ThinkingConfig = &thinkingCfg{Budget: &budget}
	} else if budget, ok := thinkingBudgetFrom(src.ReasoningEffort); ok {
		out.GenerationConfig.ThinkingConfig = &thinkingCfg{Budget: &budget}
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
	applyResponseFormat(out.GenerationConfig, src.ResponseFormat)
	if decls := geminiToolDeclarations(src.Tools); len(decls) > 0 {
		out.Tools = []geminiTool{{FunctionDeclarations: decls}}
		out.ToolConfig = geminiToolConfigFrom(src.ToolChoice)
	}

	// Tool call ids are opaque to Gemini; functionResponse is keyed by the
	// function name, so resolve every id up front.
	toolNames := map[string]string{}
	for _, m := range src.Messages {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && tc.Function.Name != "" {
				toolNames[tc.ID] = tc.Function.Name
			}
		}
	}

	for _, m := range src.Messages {
		if m.Role == "system" {
			// Gemini carries the system prompt out of band from the turn list;
			// several system messages merge into one instruction.
			if out.SystemInstruction == nil {
				out.SystemInstruction = &geminiContent{}
			}
			if text := flattenText(m.Content); text != "" {
				out.SystemInstruction.Parts = append(out.SystemInstruction.Parts, geminiPart{Text: text})
			}
			continue
		}

		// Gemini names roles "user" and "model"; "assistant" maps to "model"
		// and a tool result is a user-role functionResponse part.
		role := "user"
		var parts []geminiPart
		switch m.Role {
		case "assistant":
			role = "model"
			parts = convertGeminiParts(m.Content)
			for _, tc := range m.ToolCalls {
				parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{
					Name: tc.Function.Name,
					Args: toolArguments(tc.Function.Arguments),
				}})
			}
		case "tool", "function":
			parts = []geminiPart{{FunctionResponse: &geminiFunctionResult{
				Name:     firstNonEmpty(m.Name, toolNames[m.ToolCallID], m.ToolCallID, "function"),
				Response: toolResultPayload(m.Content),
			}}}
		default:
			parts = convertGeminiParts(m.Content)
		}
		if len(parts) == 0 {
			continue
		}
		// Gemini expects user/model turns to alternate, so adjacent same-role
		// messages (parallel tool results, for example) become one turn.
		if n := len(out.Contents); n > 0 && out.Contents[n-1].Role == role {
			out.Contents[n-1].Parts = append(out.Contents[n-1].Parts, parts...)
			continue
		}
		out.Contents = append(out.Contents, geminiContent{Role: role, Parts: parts})
	}
	return out, nil
}

// applyResponseFormat translates OpenAI's response_format into Gemini's JSON
// mode: json_object sets the MIME type, and json_schema also forwards the
// caller's schema unchanged through responseJsonSchema.
func applyResponseFormat(cfg *generationCfg, format *chatResponseFormat) {
	if format == nil {
		return
	}
	switch format.Type {
	case "json_object":
		cfg.ResponseMimeType = "application/json"
	case "json_schema":
		if format.JSONSchema == nil || len(format.JSONSchema.Schema) == 0 || string(format.JSONSchema.Schema) == "null" {
			return
		}
		cfg.ResponseMimeType = "application/json"
		cfg.ResponseJSONSchema = format.JSONSchema.Schema
	}
}

// geminiToolDeclarations translates OpenAI function tools. Non-function tools
// (built-in browser/search tools in some APIs) have no Gemini equivalent here
// and are skipped rather than sent as malformed declarations.
func geminiToolDeclarations(tools []chatTool) []geminiFunctionDecl {
	var decls []geminiFunctionDecl
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		if t.Function.Name == "" {
			continue
		}
		decl := geminiFunctionDecl{Name: t.Function.Name, Description: t.Function.Description}
		if len(t.Function.Parameters) > 0 && string(t.Function.Parameters) != "null" {
			decl.ParametersJSONSchema = t.Function.Parameters
		}
		decls = append(decls, decl)
	}
	return decls
}

// geminiToolConfigFrom maps OpenAI's tool_choice onto Gemini's calling mode.
// AUTO is Gemini's default, so it needs no config at all.
func geminiToolConfigFrom(choice any) *geminiToolConfig {
	switch v := choice.(type) {
	case string:
		switch v {
		case "none":
			return &geminiToolConfig{FunctionCallingConfig: geminiFunctionCallingConfig{Mode: "NONE"}}
		case "required":
			return &geminiToolConfig{FunctionCallingConfig: geminiFunctionCallingConfig{Mode: "ANY"}}
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
		return &geminiToolConfig{FunctionCallingConfig: geminiFunctionCallingConfig{
			Mode:                 "ANY",
			AllowedFunctionNames: []string{name},
		}}
	}
	return nil
}

// toolArguments normalizes OpenAI's argument string (a JSON document encoded
// as a string) into the JSON object Gemini's functionCall.args requires.
func toolArguments(raw string) json.RawMessage {
	if raw == "" {
		return json.RawMessage(`{}`)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err == nil {
		if obj == nil {
			// "null" is a legal argument string but Gemini's args is a
			// Struct, which null is not.
			return json.RawMessage(`{}`)
		}
		return json.RawMessage(raw)
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err == nil {
		wrapped, _ := json.Marshal(map[string]any{"value": value})
		return wrapped
	}
	return json.RawMessage(`{}`)
}

// toolResultPayload renders an OpenAI tool result as the JSON object Gemini
// requires. Tool output is usually a JSON object encoded as a string; that
// object is passed through, anything else is wrapped in {"result": ...}.
func toolResultPayload(raw json.RawMessage) json.RawMessage {
	text := flattenText(raw)
	if text == "" {
		return json.RawMessage(`{}`)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &obj); err == nil && obj != nil {
		return json.RawMessage(text)
	}
	var value any
	if err := json.Unmarshal([]byte(text), &value); err == nil {
		wrapped, _ := json.Marshal(map[string]any{"result": value})
		return wrapped
	}
	wrapped, _ := json.Marshal(map[string]any{"result": text})
	return wrapped
}

// convertGeminiParts maps OpenAI content onto Gemini parts, including base64
// inline media carried in data URIs.
func convertGeminiParts(raw json.RawMessage) []geminiPart {
	// An assistant turn that only called tools carries a null or empty
	// content; emitting a blank text part would be rejected by Gemini.
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
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
		case "image_url", "input_image":
			// The responses shape names the part input_image but carries
			// the same URL field; both forms are inline media here.
			if p.ImageURL == nil {
				continue
			}
			mediaType, data, ok := splitDataURI(p.ImageURL.URL)
			if !ok {
				// A remote URL has no inline payload; Gemini only accepts
				// base64 media here, so it is dropped, not mislabelled.
				continue
			}
			out = append(out, geminiPart{InlineData: &inlineMedia{MimeType: mediaType, Data: data}})
		case "input_audio":
			if p.InputAudio == nil || p.InputAudio.Data == "" {
				continue
			}
			mime := "audio/" + p.InputAudio.Format
			if mime == "audio/mp3" {
				mime = "audio/mpeg" // Gemini's canonical mp3 MIME type
			}
			out = append(out, geminiPart{InlineData: &inlineMedia{MimeType: mime, Data: p.InputAudio.Data}})
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
	var calls []map[string]any
	if len(g.Candidates) > 0 {
		finish = g.Candidates[0].FinishReason
		for _, p := range g.Candidates[0].Content.Parts {
			if p.Thought {
				continue
			}
			content.WriteString(p.Text)
			if p.FunctionCall != nil {
				calls = append(calls, openAIToolCall(p.FunctionCall))
			}
		}
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if len(calls) > 0 {
		message["tool_calls"] = calls
		if content.Len() == 0 {
			message["content"] = nil
		}
	}
	reason := mapGeminiFinish(finish)
	if len(calls) > 0 && reason == "stop" {
		reason = "tool_calls"
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
			"message":       message,
			"finish_reason": reason,
		}},
		"usage": map[string]any{
			"prompt_tokens":     g.UsageMetadata.PromptTokenCount,
			"completion_tokens": g.UsageMetadata.CandidatesTokenCount,
			"total_tokens":      total,
		},
	}
}

// openAIToolCall renders one Gemini function call in OpenAI's tool_call shape.
// Gemini returns arguments as a JSON object; OpenAI carries them as a string.
func openAIToolCall(call *geminiFunctionCall) map[string]any {
	args := "{}"
	if len(call.Args) > 0 {
		args = string(call.Args)
	}
	return map[string]any{
		"id":   "call_" + newID(),
		"type": "function",
		"function": map[string]any{
			"name":      call.Name,
			"arguments": args,
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

	httpReq, err := a.client.BuildRequest(ctx, "POST", url, bytes.NewReader(encoded), a.headers())
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

	httpReq, err := a.client.BuildRequest(ctx, "POST", url, bytes.NewReader(encoded), headers)
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
	// toolIndex tracks the message-wide position of streamed tool calls, so
	// deltas that arrive over several chunks keep distinct indexes.
	toolIndex int
	// sawToolCalls upgrades the terminal finish_reason to tool_calls even
	// when a later chunk carries only the finish reason.
	sawToolCalls bool
}

// geminiChunkParts extracts candidate 0's text, finish reason and function
// calls from one streamed generateContent payload.
func geminiChunkParts(decoded geminiResponse) (string, string, []*geminiFunctionCall) {
	var text strings.Builder
	var finish string
	var calls []*geminiFunctionCall
	if len(decoded.Candidates) > 0 {
		finish = decoded.Candidates[0].FinishReason
		for _, p := range decoded.Candidates[0].Content.Parts {
			if p.Thought {
				continue
			}
			text.WriteString(p.Text)
			if p.FunctionCall != nil {
				calls = append(calls, p.FunctionCall)
			}
		}
	}
	return text.String(), finish, calls
}

// streamChunkPayload builds an OpenAI chat.completion.chunk body out of a text
// delta, tool calls and/or a finish reason, mapping Gemini's finish vocabulary.
// startIndex is where this batch of tool calls begins in the message, and
// hadCalls reports whether the message already streamed any tool call.
func streamChunkPayload(model string, text string, finish string, calls []*geminiFunctionCall, startIndex int, hadCalls bool) ([]byte, bool) {
	if text == "" && finish == "" && len(calls) == 0 {
		return nil, false
	}
	delta := map[string]any{}
	if text != "" {
		delta["content"] = text
	}
	if len(calls) > 0 {
		toolCalls := make([]map[string]any, 0, len(calls))
		for i, call := range calls {
			tc := openAIToolCall(call)
			tc["index"] = startIndex + i
			toolCalls = append(toolCalls, tc)
		}
		delta["tool_calls"] = toolCalls
	}
	reason := mapGeminiFinish(finish)
	if (len(calls) > 0 || hadCalls) && reason == "stop" {
		reason = "tool_calls"
	}
	choice := map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": nil,
	}
	if finish != "" {
		choice["finish_reason"] = reason
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
		text, finish, calls := geminiChunkParts(decoded)
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
		hadCalls := s.sawToolCalls || len(calls) > 0
		if data, ok := streamChunkPayload(s.model, text, finish, calls, s.toolIndex, hadCalls); ok {
			out.Data = data
			if len(calls) > 0 {
				s.sawToolCalls = true
				s.toolIndex += len(calls)
			}
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

	httpReq, err := a.client.BuildRequest(ctx, "POST", url, bytes.NewReader(encoded), a.headers())
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
