// Package providers adapts upstream LLM services to the OpenAI wire format.
//
// Adapters are resolved at compile time from a registry keyed by provider slug.
// Adding a provider that speaks the OpenAI protocol needs no code change at
// all: configure its base URL in the dashboard and it is served by the
// OpenAI-compatible adapter. Only a genuinely different wire protocol requires
// a new adapter and a rebuild.
//
// The Python version additionally scanned providers/custom/*.py at startup. Go
// cannot load code at runtime without cgo or a subprocess RPC protocol, and the
// Python loader never actually registered the modules it loaded, so the feature
// did not work in practice.
package providers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Capability describes what a provider supports, so the proxy can reject an
// unsupported request before spending a round trip.
type Capability struct {
	Images     bool
	Embeddings bool
	STT        bool
	TTS        bool
}

// Request carries everything an adapter needs to make one upstream call.
type Request struct {
	// Body is the raw JSON request body, already carrying the caller's fields.
	Body []byte
	// Model is the provider-qualified model string, e.g. "openai/gpt-4".
	Model string
	// APIKey is the credential to present upstream.
	APIKey string
	// BaseURL overrides the adapter's default endpoint.
	BaseURL string
	// Stream requests a streamed response.
	Stream bool
}

// Response is a non-streaming upstream reply.
type Response struct {
	StatusCode  int
	Header      http.Header
	Body        []byte
	ContentType string
}

// Chunk is one server-sent event payload from a streaming response, already
// normalised to OpenAI's chat.completion.chunk shape.
type Chunk struct {
	// Data is the JSON payload, without the "data: " prefix.
	Data []byte
	// Usage is set on the final chunk when the provider reports token counts.
	Usage *Usage
	// Done marks the terminating [DONE] sentinel.
	Done bool
}

// Stream is an in-flight streaming response.
type Stream interface {
	// Next returns the next chunk, or io.EOF when the provider is finished.
	Next() (*Chunk, error)
	// Close releases the underlying connection. It is safe to call after an
	// error or a client disconnect.
	Close() error
}

// Usage is a token accounting summary.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Adapter is one upstream protocol implementation.
//
// Implementations must be safe for concurrent use and must not mutate the
// Request they are given.
type Adapter interface {
	// Name is the provider slug this adapter serves.
	Name() string

	// Capabilities reports what the adapter supports in general. Per-model
	// support is tracked separately in the models table.
	Capabilities() Capability

	// Complete performs a buffered chat completion or completion request.
	Complete(ctx context.Context, req Request) (*Response, error)

	// Stream performs a streaming completion. The returned Stream owns the
	// response body and must be closed by the caller.
	Stream(ctx context.Context, req Request) (Stream, error)

	// Embeddings performs an embedding request.
	Embeddings(ctx context.Context, req Request) (*Response, error)

	// Transcribe performs speech-to-text over raw audio bytes.
	Transcribe(ctx context.Context, req Request, audio AudioFile) (*Response, error)

	// Speak performs text-to-speech.
	Speak(ctx context.Context, req Request) (*Response, error)

	// Probe detects what a specific model supports. It is best-effort and
	// must never fail the caller; return the zero value on error.
	Probe(ctx context.Context, req Request) (ModelCapability, error)
}

// AudioFile is an uploaded audio payload for transcription.
type AudioFile struct {
	Filename    string
	ContentType string
	Data        []byte
}

// ModelCapability is the result of probing one model.
type ModelCapability struct {
	SupportsTools  bool `json:"supports_tools"`
	SupportsVision bool `json:"supports_vision"`
	SupportsJSON   bool `json:"supports_json"`
	SupportsStream bool `json:"supports_stream"`
	MaxContext     int  `json:"max_context,omitempty"`
	MaxOutput      int  `json:"max_output,omitempty"`
}

// EndpointError reports a non-success upstream response.
type EndpointError struct {
	StatusCode int
	Body       []byte
	Provider   string
}

func (e *EndpointError) Error() string {
	body := string(e.Body)
	if len(body) > 200 {
		body = body[:200]
	}
	return fmt.Sprintf("providers: %s returned %d: %s", e.Provider, e.StatusCode, body)
}

// Errors distinguishing the failure modes the retry loop distinguishes.
var (
	// ErrUnsupported means the adapter cannot serve this request type at all.
	ErrUnsupported = errors.New("providers: capability not supported by this adapter")
	// ErrInvalidRequest means the request body is not usable.
	ErrInvalidRequest = errors.New("providers: invalid request")
)

// --- shared HTTP plumbing ---

// Client is the HTTP client used for all upstream calls. A single client with
// tuned transport pools connections across requests; the Python version kept a
// separate httpx.AsyncClient per provider slug.
type Client struct {
	http *http.Client
}

// NewClient builds an upstream client with connection pooling tuned for
// long-lived streaming responses.
func NewClient() *Client {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * secondDuration,
		// Streaming responses can legitimately hold a connection open for
		// minutes, so ResponseHeaderTimeout is the bound that matters.
		ResponseHeaderTimeout: 60 * secondDuration,
	}
	return &Client{http: &http.Client{Transport: transport}}
}

// Do executes an upstream request.
func (c *Client) Do(req *http.Request) (*http.Response, error) { return c.http.Do(req) }

// BuildRequest assembles an upstream request without sending it.
func (c *Client) BuildRequest(ctx context.Context, method, rawURL string, body io.Reader, headers map[string]string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// joinURL appends a path to a base URL, tolerating a missing or duplicated
// slash.
func joinURL(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

// stripProviderPrefix removes a leading "provider/" from a model string.
func stripProviderPrefix(model string) string {
	if i := strings.Index(model, "/"); i >= 0 {
		return model[i+1:]
	}
	return model
}

// peekFields decodes only the fields the proxy needs from a request body,
// without imposing a schema on the rest of it.
func peekFields(body []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return fields, nil
}

// StringField reads a string field, reporting whether it was present.
func StringField(fields map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := fields[name]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// BoolField reads a boolean field, reporting whether it was present and was
// actually a boolean. Some clients send numbers or strings for these flags, so
// those forms are accepted too.
func BoolField(fields map[string]json.RawMessage, name string) (bool, bool) {
	raw, ok := fields[name]
	if !ok {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, true
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n != 0, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s == "true", true
	}
	return false, false
}

// rewriteModel replaces the "model" field in a JSON body, preserving every
// other field verbatim. The proxy must not re-encode the body, because doing
// so would silently drop provider-specific extensions it does not model.
func rewriteModel(body []byte, model string) ([]byte, error) {
	fields, err := peekFields(body)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	fields["model"] = encoded
	return json.Marshal(fields)
}

// HasImageInput reports whether a chat request carries an image part, which
// determines whether the target model must declare vision support.
func HasImageInput(body []byte) bool {
	fields, err := peekFields(body)
	if err != nil {
		return false
	}
	raw, ok := fields["messages"]
	if !ok {
		return false
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return false
	}
	for _, m := range messages {
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(m, &msg); err != nil {
			continue
		}
		// Multi-part content is the only shape that can carry an image.
		var parts []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(msg.Content, &parts); err != nil {
			continue
		}
		for _, p := range parts {
			if p.Type == "image_url" || p.Type == "input_image" {
				return true
			}
		}
	}
	return false
}

// sseStream reads an OpenAI-shaped server-sent event stream.
//
// It is shared by every adapter that already speaks SSE, so the Anthropic and
// Gemini translators only have to produce OpenAI-shaped chunks and can reuse
// this reader.
type sseStream struct {
	scanner *bufio.Scanner
	body    io.ReadCloser
	done    bool
}

func newSSEStream(body io.ReadCloser) *sseStream {
	sc := bufio.NewScanner(body)
	// SSE frames can exceed bufio's default 64KiB cap when a tool call or a
	// long completion delta arrives.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &sseStream{scanner: sc, body: body}
}

func (s *sseStream) Next() (*Chunk, error) {
	for {
		if s.done {
			return nil, io.EOF
		}
		line, err := s.readLine()
		if err != nil {
			if err == io.EOF {
				s.done = true
				return nil, io.EOF
			}
			return nil, err
		}
		payload, ok := parseSSELine(line)
		if !ok {
			continue
		}
		if string(payload) == "[DONE]" {
			s.done = true
			return &Chunk{Done: true}, nil
		}
		chunk := &Chunk{Data: payload}
		if u := extractUsage(payload); u != nil {
			chunk.Usage = u
		}
		return chunk, nil
	}
}

// readLine returns the next line, transparently joining the blank-line
// separator that terminates each SSE frame.
func (s *sseStream) readLine() ([]byte, error) {
	if !s.scanner.Scan() {
		if err := s.scanner.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	return s.scanner.Bytes(), nil
}

func (s *sseStream) Close() error { return s.body.Close() }

// parseSSELine extracts the payload of a "data:" line.
func parseSSELine(line []byte) ([]byte, bool) {
	// Frame separators and keepalive comments carry no payload.
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" || strings.HasPrefix(trimmed, ":") {
		return nil, false
	}
	if !strings.HasPrefix(trimmed, "data:") {
		return nil, false
	}
	return []byte(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))), true
}

// extractUsage pulls a usage object out of a chunk payload, if present.
func extractUsage(payload []byte) *Usage {
	if !containsSubstring(payload, "usage") {
		return nil
	}
	var envelope struct {
		Usage *Usage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Usage == nil {
		return nil
	}
	u := *envelope.Usage
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	return &u
}

func containsSubstring(haystack []byte, needle string) bool {
	return len(haystack) > 0 && strings.Contains(string(haystack), needle)
}

// validateBaseURL rejects endpoints that are not http(s), so a malformed
// admin-entered base URL cannot redirect traffic to an unexpected scheme.
func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("providers: invalid base URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("providers: base URL %q must be http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("providers: base URL %q has no host", raw)
	}
	return nil
}

// readAllLimited reads a bounded amount from r. Upstream error pages can be
// large or unbounded; a proxy must not buffer them without limit.
func readAllLimited(r io.Reader) ([]byte, error) {
	const maxBody = 32 << 20 // 32 MiB
	return io.ReadAll(io.LimitReader(r, maxBody))
}

// decodeJSON unmarshals into v, wrapping the error with the provider context.
func decodeJSON(body []byte, v any) error {
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("providers: decode response: %w", err)
	}
	return nil
}
