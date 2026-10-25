package providers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Protocol selects the wire format a configured provider speaks.
type Protocol string

const (
	// ProtocolOpenAI is the OpenAI chat protocol. Every major LLM vendor
	// exposes it, including self-hosted servers, so this covers the overwhelming
	// majority of providers.
	ProtocolOpenAI Protocol = "openai"
	// ProtocolAnthropic is Anthropic's Messages API.
	ProtocolAnthropic Protocol = "anthropic"
	// ProtocolGemini is Google's generateContent API.
	ProtocolGemini Protocol = "gemini"
)

// Spec is a compile-time provider definition.
//
// Adding one of these requires a rebuild, which is why the set is deliberately
// tiny: OpenAI-compatible endpoints are configured at runtime instead, via
// Definition.
type Spec struct {
	// Slug is the provider name matched against a model's provider prefix.
	Slug string
	// DisplayName is a human-readable label.
	DisplayName string
	// BaseURL is the default upstream.
	BaseURL string
	// Protocol selects the adapter implementation.
	Protocol Protocol
	// Auth is how the credential is attached.
	Auth AuthStyle
	// AuthHeader is the header name for AuthHeaderKey.
	AuthHeader string
	// QueryParam is the query parameter name for AuthQueryKey.
	QueryParam string
	// Capabilities declares what the provider supports.
	Capabilities Capability
	// Probeable marks providers whose model capabilities can be detected.
	Probeable bool
}

// Definition is a runtime-configured provider, loaded from provider.yaml.
//
// This is the mechanism that removes recompilation from the common case: a
// provider that speaks one of the three known protocols is configured with a
// base URL. No code change, no build.
type Definition struct {
	Slug         string
	DisplayName  string
	BaseURL      string
	Protocol     Protocol
	Auth         AuthStyle
	AuthHeader   string
	QueryParam   string
	Capabilities Capability
}

// builtin lists the protocols compiled into the binary. It is intentionally
// small: everything else is configured at runtime through Definition.
var builtin = []Spec{
	{Slug: "openai", DisplayName: "OpenAI", BaseURL: DefaultBaseURL, Protocol: ProtocolOpenAI,
		Auth: AuthBearer, Capabilities: Capability{Images: true, Embeddings: true, STT: true, TTS: true}, Probeable: true},
	{Slug: "anthropic", DisplayName: "Anthropic", BaseURL: AnthropicBaseURL, Protocol: ProtocolAnthropic,
		Auth: AuthHeaderKey, AuthHeader: "x-api-key", Capabilities: Capability{Images: true}, Probeable: true},
	{Slug: "gemini", DisplayName: "Google AI Studio", BaseURL: GeminiBaseURL, Protocol: ProtocolGemini,
		Auth: AuthQueryKey, QueryParam: "key", Capabilities: Capability{Images: true, Embeddings: true}, Probeable: true},
}

// catalog is the built-in provider list returned by Catalog.
var catalog = []map[string]any{
	{"id": "openai", "name": "OpenAI", "category": "cloud", "base_url": DefaultBaseURL,
		"auth": "bearer", "fields": []string{"api_key"}, "capabilities": []string{"Text", "Vision", "Image", "Embeddings", "STT", "TTS"}},
	{"id": "anthropic", "name": "Anthropic", "category": "cloud", "base_url": AnthropicBaseURL,
		"auth": "x-api-key", "fields": []string{"api_key"}, "capabilities": []string{"Text", "Vision"}},
	{"id": "gemini", "name": "Google AI Studio", "category": "cloud", "base_url": GeminiBaseURL,
		"auth": "query_key", "fields": []string{"api_key"}, "capabilities": []string{"Text", "Vision", "Embeddings"}},
}

// Catalog returns the built-in provider list.
func Catalog() []map[string]any { return catalog }

// Registry resolves provider slugs to adapters.
//
// Built-in specs take precedence over runtime definitions, so a configuration
// row can never shadow a compiled provider and change its behaviour silently.
type Registry struct {
	client *Client

	mu      sync.RWMutex
	builtin map[string]Spec
	defined map[string]Definition
}

// NewRegistry builds a registry seeded with the built-in providers.
func NewRegistry(client *Client) *Registry {
	if client == nil {
		client = NewClient()
	}
	r := &Registry{
		client:  client,
		builtin: make(map[string]Spec, len(builtin)),
		defined: map[string]Definition{},
	}
	for _, spec := range builtin {
		r.builtin[spec.Slug] = spec
	}
	return r
}

// Define registers a runtime-configured provider; it takes effect
// immediately for subsequent lookups.
func (r *Registry) Define(d Definition) error {
	if d.Slug == "" {
		return fmt.Errorf("providers: definition needs a slug")
	}
	if err := validateBaseURL(d.BaseURL); err != nil {
		return err
	}
	switch d.Protocol {
	case ProtocolOpenAI, ProtocolAnthropic, ProtocolGemini:
	case "":
		d.Protocol = ProtocolOpenAI
	default:
		return fmt.Errorf("providers: unknown protocol %q", d.Protocol)
	}
	if d.Auth == "" {
		d.Auth = AuthBearer
	}
	if d.Auth == AuthHeaderKey && d.AuthHeader == "" {
		d.AuthHeader = "x-api-key"
	}
	if d.Auth == AuthQueryKey && d.QueryParam == "" {
		d.QueryParam = "key"
	}
	if d.DisplayName == "" {
		d.DisplayName = d.Slug
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.defined[strings.ToLower(d.Slug)] = d
	return nil
}

// Remove drops a runtime-configured provider.
func (r *Registry) Remove(slug string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.defined, strings.ToLower(slug))
}

// Definitions returns the runtime-configured providers, sorted by slug.
func (r *Registry) Definitions() []Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Definition, 0, len(r.defined))
	for _, d := range r.defined {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// Known reports whether slug resolves to any adapter.
func (r *Registry) Known(slug string) bool {
	_, err := r.Lookup(slug)
	return err == nil
}

// Lookup returns the spec for a slug, from built-ins or definitions.
func (r *Registry) Lookup(slug string) (Spec, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))

	r.mu.RLock()
	spec, ok := r.builtin[slug]
	def, isDefined := r.defined[slug]
	r.mu.RUnlock()

	if ok {
		return spec, nil
	}
	if isDefined {
		return Spec{
			Slug:         def.Slug,
			DisplayName:  def.DisplayName,
			BaseURL:      def.BaseURL,
			Protocol:     def.Protocol,
			Auth:         def.Auth,
			AuthHeader:   def.AuthHeader,
			QueryParam:   def.QueryParam,
			Capabilities: def.Capabilities,
		}, nil
	}
	return Spec{}, fmt.Errorf("providers: unknown provider %q", slug)
}

// Adapter builds the adapter serving slug, or an OpenAI-protocol adapter when
// slug is unknown.
//
// The fallback is what makes new providers free: any endpoint that speaks the
// OpenAI protocol works the moment its base URL is configured, with no
// registration at all.
func (r *Registry) Adapter(slug string) Adapter {
	spec, err := r.Lookup(slug)
	if err != nil {
		// Unknown slug, OpenAI-compatible endpoint.
		return NewOpenAIAdapter("", AuthHeaderNames{Auth: AuthBearer}, r.client)
	}
	return r.adapterFor(spec)
}

func (r *Registry) adapterFor(spec Spec) Adapter {
	base := spec.BaseURL
	var extra map[string]string

	switch spec.Protocol {
	case ProtocolAnthropic:
		extra = map[string]string{"anthropic-version": "2023-06-01"}
		return NewAnthropicAdapter(base, extra, r.client)
	case ProtocolGemini:
		return NewGeminiAdapter(base, extra, r.client)
	default:
		return NewOpenAIAdapter(base, AuthHeaderNames{
			Auth:       spec.Auth,
			Header:     spec.AuthHeader,
			QueryParam: spec.QueryParam,
			Extra:      extra,
		}, r.client)
	}
}

// AdapterFor returns an adapter honouring a per-request base URL override.
func (r *Registry) AdapterFor(slug, baseURLOverride string) Adapter {
	a := r.Adapter(slug)
	if baseURLOverride == "" {
		return a
	}
	// Only the OpenAI-protocol adapter accepts a runtime base URL; the other
	// protocols bake theirs in at construction because their URLs embed the
	// model name and auth style.
	if oa, ok := a.(*OpenAIAdapter); ok {
		clone := *oa
		clone.BaseURL = strings.TrimRight(baseURLOverride, "/")
		return &clone
	}
	return a
}

// FetchModels lists the models a provider exposes, using each provider's own
// discovery endpoint where one exists.
func (r *Registry) FetchModels(ctx context.Context, slug, apiKey, baseURL string) ([]ModelInfo, error) {
	spec, err := r.Lookup(slug)
	if err != nil {
		// Unknown slugs are assumed OpenAI-compatible.
		spec = Spec{Protocol: ProtocolOpenAI, Auth: AuthBearer}
	}
	if baseURL != "" {
		if err := validateBaseURL(baseURL); err != nil {
			return nil, err
		}
	}

	switch spec.Protocol {
	case ProtocolAnthropic:
		// Anthropic publishes no model listing endpoint, so the known model
		// families are returned instead of contacting the API.
		return anthropicKnownModels(), nil
	case ProtocolGemini:
		return r.fetchGeminiModels(ctx, spec, apiKey, baseURL)
	default:
		return r.fetchOpenAIModels(ctx, spec, apiKey, baseURL)
	}
}

// ModelInfo is one model advertised by a provider.
type ModelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (r *Registry) fetchOpenAIModels(ctx context.Context, spec Spec, apiKey, baseURL string) ([]ModelInfo, error) {
	base := baseURL
	if base == "" {
		base = spec.BaseURL
	}
	if base == "" {
		base = DefaultBaseURL
	}
	url := modelsURL(base)

	headers := map[string]string{}
	switch spec.Auth {
	case AuthHeaderKey:
		name := spec.AuthHeader
		if name == "" {
			name = "x-api-key"
		}
		headers[name] = apiKey
	case AuthQueryKey:
		param := spec.QueryParam
		if param == "" {
			param = "key"
		}
		url += "&" + param + "=" + apiKey
	default:
		if spec.Auth != AuthNone {
			headers["Authorization"] = "Bearer " + apiKey
		}
	}

	body, err := r.getJSON(ctx, url, headers, "openai")
	if err != nil {
		return nil, err
	}
	// The OpenAI listing shape is also used by most compatible servers.
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := decodeJSON(body, &decoded); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(decoded.Data))
	for _, m := range decoded.Data {
		if m.ID == "" {
			continue
		}
		out = append(out, ModelInfo{ID: m.ID, Name: m.ID})
	}
	return out, nil
}

func (r *Registry) fetchGeminiModels(ctx context.Context, spec Spec, apiKey, baseURL string) ([]ModelInfo, error) {
	base := baseURL
	if base == "" {
		base = spec.BaseURL
	}
	if base == "" {
		base = GeminiBaseURL
	}
	// Google appends the key as a query parameter rather than a header.
	url := strings.TrimRight(base, "/") + "/models?key=" + apiKey

	body, err := r.getJSON(ctx, url, map[string]string{}, "gemini")
	if err != nil {
		return nil, err
	}
	var decoded struct {
		Models []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		} `json:"models"`
	}
	if err := decodeJSON(body, &decoded); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(decoded.Models))
	for _, m := range decoded.Models {
		id := strings.TrimPrefix(m.Name, "models/")
		if id == "" {
			continue
		}
		name := m.DisplayName
		if name == "" {
			name = id
		}
		out = append(out, ModelInfo{ID: id, Name: name})
	}
	return out, nil
}

// getJSON performs a GET and classifies failures by status code.
func (r *Registry) getJSON(ctx context.Context, url string, headers map[string]string, provider string) ([]byte, error) {
	req, err := r.client.BuildRequest(ctx, "GET", url, nil, headers)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := readAllLimited(resp.Body)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == 401:
		return nil, fmt.Errorf("providers: %s rejected the API key", provider)
	case resp.StatusCode == 403:
		return nil, fmt.Errorf("providers: %s denied access; the account may be suspended", provider)
	case resp.StatusCode == 402:
		return nil, fmt.Errorf("providers: %s reports the quota exhausted", provider)
	case resp.StatusCode >= 400:
		return nil, &EndpointError{StatusCode: resp.StatusCode, Body: body, Provider: provider}
	}
	return body, nil
}

// modelsURL builds the models listing URL, tolerating a base that already
// carries an API version prefix.
func modelsURL(base string) string {
	root := strings.TrimRight(base, "/")
	if strings.HasSuffix(root, "/v1") {
		return root + "/models"
	}
	return root + "/v1/models"
}

func anthropicKnownModels() []ModelInfo {
	names := []string{
		"claude-opus-4-20250514",
		"claude-sonnet-4-20250514",
		"claude-3-5-haiku-20241022",
		"claude-3-5-sonnet-20241022",
		"claude-3-opus-20240229",
	}
	out := make([]ModelInfo, 0, len(names))
	for _, n := range names {
		out = append(out, ModelInfo{ID: n, Name: n})
	}
	return out
}

var _ Adapter = (*OpenAIAdapter)(nil)
