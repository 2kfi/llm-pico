package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
)

// DefaultBaseURL is the upstream used when a model configures no api_base.
const DefaultBaseURL = "https://api.openai.com/v1"

// AuthStyle selects how a credential is attached to an upstream request.
type AuthStyle string

const (
	// AuthBearer sends Authorization: Bearer <key>.
	AuthBearer AuthStyle = "bearer"
	// AuthHeaderKey sends the key in a named header, used by Anthropic
	// (x-api-key) and others.
	AuthHeaderKey AuthStyle = "header_key"
	// AuthQueryKey appends the key as a URL query parameter, used by Google.
	AuthQueryKey AuthStyle = "query_key"
	// AuthNone sends no credential.
	AuthNone AuthStyle = "none"
)

// OpenAIAdapter speaks the OpenAI wire protocol.
//
// This is the universal adapter: every provider that exposes an
// OpenAI-compatible endpoint is served by this type with a different BaseURL
// and API key. Groq, OpenRouter, Together, DeepSeek, Mistral, xAI, Perplexity,
// vLLM, Ollama, LM Studio and any future one all route here with no code
// change and no rebuild.
type OpenAIAdapter struct {
	// BaseURL overrides DefaultBaseURL when non-empty.
	BaseURL string
	// AuthHeader is the header name used when Auth is AuthHeaderKey.
	AuthHeader string
	// QueryParam is the query parameter name used when Auth is AuthQueryKey.
	QueryParam string
	// ExtraHeaders are merged into every upstream request.
	ExtraHeaders map[string]string

	client *Client
}

// NewOpenAIAdapter builds an OpenAI-protocol adapter.
func NewOpenAIAdapter(baseURL string, key AuthHeaderNames, client *Client) *OpenAIAdapter {
	if client == nil {
		client = NewClient()
	}
	base := baseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return &OpenAIAdapter{
		BaseURL:      strings.TrimRight(base, "/"),
		AuthHeader:   key.Header,
		QueryParam:   key.QueryParam,
		ExtraHeaders: key.Extra,
		client:       client,
	}
}

// AuthHeaderNames bundles the credential-placement details a configured
// provider needs.
type AuthHeaderNames struct {
	Auth       AuthStyle
	Header     string
	QueryParam string
	Extra      map[string]string
}

func (a *OpenAIAdapter) Name() string { return "openai" }

func (a *OpenAIAdapter) Capabilities() Capability {
	return Capability{Images: true, Embeddings: true, STT: true, TTS: true}
}

// authHeaders applies the configured credential style to a header map.
func (a *OpenAIAdapter) authHeaders(apiKey string, headers map[string]string) {
	for k, v := range a.ExtraHeaders {
		headers[k] = v
	}
	style := AuthStyleFromHeader(a.AuthHeader, a.QueryParam)
	if style == AuthHeaderKey {
		name := a.AuthHeader
		if name == "" {
			name = "x-api-key"
		}
		headers[name] = apiKey
		return
	}
	// Some self-hosted endpoints need no credential at all.
	if style != AuthNone {
		headers["Authorization"] = "Bearer " + apiKey
	}
}

// AuthStyleFromHeader infers the credential style from the configured names.
// A named header means header-key style; a query parameter name means query
// style; otherwise bearer.
func AuthStyleFromHeader(header, queryParam string) AuthStyle {
	switch {
	case header != "":
		return AuthHeaderKey
	case queryParam != "":
		return AuthQueryKey
	default:
		return AuthBearer
	}
}

// queryAuth injects the credential when the provider expects it in the URL.
func (a *OpenAIAdapter) queryAuth(rawURL, apiKey string) string {
	if AuthStyleFromHeader(a.AuthHeader, a.QueryParam) != AuthQueryKey {
		return rawURL
	}
	param := a.QueryParam
	if param == "" {
		param = "key"
	}
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return rawURL + sep + param + "=" + apiKey
}

func (a *OpenAIAdapter) Complete(ctx context.Context, req Request) (*Response, error) {
	body, err := a.prepareBody(req)
	if err != nil {
		return nil, err
	}
	url := joinURL(a.endpoint(req), "chat/completions")
	return a.doJSON(ctx, req, "POST", url, body)
}

func (a *OpenAIAdapter) Stream(ctx context.Context, req Request) (Stream, error) {
	body, err := a.prepareBody(req)
	if err != nil {
		return nil, err
	}
	fields, err := peekFields(body)
	if err != nil {
		return nil, err
	}
	// The caller decides streaming; the flag must be set for the provider to
	// send events at all, so an unset or false flag is a caller error rather
	// than something to paper over.
	if stream, ok := BoolField(fields, "stream"); !ok || !stream {
		return nil, fmt.Errorf("%w: stream requested but body does not enable it", ErrInvalidRequest)
	}

	url := joinURL(a.endpoint(req), "chat/completions")
	headers := map[string]string{"Content-Type": "application/json", "Accept": "text/event-stream"}
	a.authHeaders(req.APIKey, headers)

	httpReq, err := a.client.BuildRequest(ctx, "POST", a.queryAuth(url, req.APIKey), strings.NewReader(string(body)), headers)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		payload, _ := io.ReadAll(resp.Body)
		return nil, &EndpointError{StatusCode: resp.StatusCode, Body: payload, Provider: "openai"}
	}
	return newSSEStream(resp.Body), nil
}

func (a *OpenAIAdapter) Embeddings(ctx context.Context, req Request) (*Response, error) {
	body, err := a.prepareBody(req)
	if err != nil {
		return nil, err
	}
	url := joinURL(a.endpoint(req), "embeddings")
	return a.doJSON(ctx, req, "POST", url, body)
}

func (a *OpenAIAdapter) Speak(ctx context.Context, req Request) (*Response, error) {
	body, err := a.prepareBody(req)
	if err != nil {
		return nil, err
	}
	url := joinURL(a.endpoint(req), "audio/speech")
	return a.doJSON(ctx, req, "POST", url, body)
}

// Transcribe uploads audio as multipart form data, which is the only multipart
// request the proxy makes.
func (a *OpenAIAdapter) Transcribe(ctx context.Context, req Request, audio AudioFile) (*Response, error) {
	var buf strings.Builder
	mw := multipart.NewWriter(&buf)

	fields, _ := peekFields(req.Body)
	model, ok := StringField(fields, "model")
	if !ok {
		model = stripProviderPrefix(req.Model)
	}
	_ = mw.WriteField("model", stripProviderPrefix(model))

	// Forward the optional tuning fields OpenAI accepts on the endpoint.
	for _, name := range []string{"response_format", "language", "temperature", "timestamp_granularities", "prompt"} {
		if v, ok := StringField(fields, name); ok && v != "" {
			_ = mw.WriteField(name, v)
		}
	}

	part, err := mw.CreateFormFile("file", audio.Filename)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(audio.Data); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	headers := map[string]string{"Content-Type": mw.FormDataContentType()}
	a.authHeaders(req.APIKey, headers)

	httpReq, err := a.client.BuildRequest(ctx, "POST", joinURL(a.endpoint(req), "audio/transcriptions"), strings.NewReader(buf.String()), headers)
	if err != nil {
		return nil, err
	}
	return a.finish(ctx, httpReq, req, "openai")
}

// prepareBody rewrites the model field to its upstream form and normalises the
// max_tokens parameter for model families that require a different name.
func (a *OpenAIAdapter) prepareBody(req Request) ([]byte, error) {
	upstream := stripProviderPrefix(req.Model)
	body, err := rewriteModel(req.Body, upstream)
	if err != nil {
		return nil, err
	}
	return a.normalizeParams(body, upstream)
}

// normalizeParams maps max_tokens to max_completion_tokens for the newer
// OpenAI model families that reject the old name, and applies the reasoning
// effort rewrite those families require.
func (a *OpenAIAdapter) normalizeParams(body []byte, model string) ([]byte, error) {
	if !requiresMaxCompletionTokens(model) {
		return body, nil
	}
	fields, err := peekFields(body)
	if err != nil {
		return nil, err
	}
	if raw, ok := fields["max_tokens"]; ok {
		// Only rename when the newer field is absent, so an explicit
		// max_completion_tokens is never clobbered.
		if _, hasNew := fields["max_completion_tokens"]; !hasNew {
			fields["max_completion_tokens"] = raw
			delete(fields, "max_tokens")
		}
	}
	return json.Marshal(fields)
}

// requiresMaxCompletionTokens reports whether a model uses the newer token
// parameter name.
func requiresMaxCompletionTokens(model string) bool {
	for _, prefix := range []string{"gpt-5", "o3", "o4"} {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

func (a *OpenAIAdapter) doJSON(ctx context.Context, req Request, method, url string, body []byte) (*Response, error) {
	headers := map[string]string{"Content-Type": "application/json"}
	a.authHeaders(req.APIKey, headers)
	httpReq, err := a.client.BuildRequest(ctx, method, a.queryAuth(url, req.APIKey), strings.NewReader(string(body)), headers)
	if err != nil {
		return nil, err
	}
	return a.finish(ctx, httpReq, req, "openai")
}

func (a *OpenAIAdapter) finish(ctx context.Context, httpReq *http.Request, req Request, provider string) (*Response, error) {
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &EndpointError{StatusCode: resp.StatusCode, Body: payload, Provider: provider}
	}
	out := &Response{
		StatusCode:  resp.StatusCode,
		Header:      resp.Header,
		Body:        payload,
		ContentType: resp.Header.Get("Content-Type"),
	}
	if req.Stream {
		out.Body = stripReasoningBlocks(out.Body)
	}
	return out, nil
}

// endpoint resolves the base URL for this request.
func (a *OpenAIAdapter) endpoint(req Request) string {
	if req.BaseURL != "" {
		return strings.TrimRight(req.BaseURL, "/")
	}
	return a.BaseURL
}

// thinkBlock matches an Anthropic-style <think>...</think> span, which some
// reasoning models emit inline and which clients do not expect to receive.
var thinkBlock = regexp.MustCompile(`(?s)<think>.*?</think>`)

// stripReasoningBlocks removes inline reasoning spans from a completion's
// message content.
func stripReasoningBlocks(body []byte) []byte {
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Choices) == 0 {
		return body
	}
	changed := false
	for i := range envelope.Choices {
		c := envelope.Choices[i].Message.Content
		if c == "" {
			continue
		}
		cleaned := strings.TrimSpace(thinkBlock.ReplaceAllString(c, ""))
		if cleaned != c {
			envelope.Choices[i].Message.Content = cleaned
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return body
	}
	return out
}

// Probe sends a minimal request with the optional features enabled and infers
// support from the error text when the provider rejects one.
func (a *OpenAIAdapter) Probe(ctx context.Context, req Request) (ModelCapability, error) {
	caps := ModelCapability{SupportsStream: true}
	body, err := json.Marshal(map[string]any{
		"model": stripProviderPrefix(req.Model),
		"messages": []map[string]string{
			{"role": "user", "content": "Hi"},
		},
		"max_tokens":      1,
		"tools":           []map[string]any{{"type": "function", "function": map[string]any{"name": "t", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}},
		"response_format": map[string]string{"type": "json_object"},
	})
	if err != nil {
		return caps, err
	}

	headers := map[string]string{"Content-Type": "application/json"}
	a.authHeaders(req.APIKey, headers)
	httpReq, err := a.client.BuildRequest(ctx, "POST", a.queryAuth(joinURL(a.endpoint(req), "chat/completions"), req.APIKey), strings.NewReader(string(body)), headers)
	if err != nil {
		return caps, err
	}
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return caps, nil // network errors are not capability signals
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		caps.SupportsTools = true
		caps.SupportsJSON = true
	case http.StatusBadRequest:
		text := strings.ToLower(string(payload))
		if strings.Contains(text, "tools") || strings.Contains(text, "function") {
			caps.SupportsTools = false
		}
		if strings.Contains(text, "response_format") {
			caps.SupportsJSON = false
		}
	}
	return caps, nil
}

var _ Adapter = (*OpenAIAdapter)(nil)
