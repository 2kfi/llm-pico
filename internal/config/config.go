// Package config holds the proxy's runtime configuration types and the file
// loader that produces them.
//
// Configuration is loaded once at startup and swapped atomically on reload,
// so in-flight requests never observe a partial update.
package config

import (
	"fmt"
	"strings"

	"github.com/arkan/llm-pico/internal/providers"
)

// Kind selects which OpenAI operation family a model serves.
type Kind string

const (
	KindChat       Kind = "chat"
	KindEmbeddings Kind = "embeddings"
	KindTTS        Kind = "tts"
	KindSTT        Kind = "stt"
)

// CircuitBreakerSettings controls when a provider is taken out of rotation.
type CircuitBreakerSettings struct {
	Enabled          bool
	FailureThreshold int
	RecoveryTimeout  int // seconds
}

// RouterSettings controls retry and failover behaviour.
type RouterSettings struct {
	NumRetries     int
	CooldownTime   int
	CircuitBreaker CircuitBreakerSettings
}

// GeneralSettings holds instance-wide settings.
type GeneralSettings struct {
	HMACEnabled bool
	HMACSecret  string
}

// ModelParams identifies the upstream credentials for one (model, provider
// slot) pair.
type ModelParams struct {
	Model   string   // model string sent upstream
	APIKey  string   // single key, when only one is configured
	APIKeys []string // rotation pool, when several are configured
	APIBase string   // may be empty: native adapters default to official endpoints
}

// ModelEntry is one model on one provider slot, plus its limit and context
// metadata. A model configured against N providers yields N entries whose
// order is the declared priority.
type ModelEntry struct {
	ModelName      string
	Kind           Kind
	Provider       string // provider.yaml slot name
	ModelParams    ModelParams
	Ctx            *int // declared context budget
	CtxOverride    *int // per-slot ctx from provider.yaml, beats Ctx
	Dims           *int // embeddings dimensions
	RPM            *int
	RPD            *int
	TPM            *int
	TPD            *int
	CPM            *int
	CPD            *int
	AudioSecPerDay *int
	AudioMin       *int

	Images     bool
	Embeddings bool
	STT        bool
	TTS        bool
}

// EffectiveCtx returns the context budget that applies to this slot: the
// provider declaration wins over the model declaration, whichever direction.
func (m *ModelEntry) EffectiveCtx() *int {
	if m.CtxOverride != nil {
		return m.CtxOverride
	}
	return m.Ctx
}

// Config is a complete, immutable snapshot of instance configuration. Once
// loaded it is never mutated; reloads build a new Config and swap the pointer.
type Config struct {
	GeneralSettings GeneralSettings
	RouterSettings  RouterSettings
	ModelList       []ModelEntry
	// ProxyKeys are the raw client bearer keys from PROXY_KEYS.
	ProxyKeys       []string
	DegradationMode string
	// providers are the provider.yaml slots, in file order.
	providers []ProviderSlot
}

// ProviderSlot is one provider.yaml entry as loaded.
type ProviderSlot struct {
	Name    string
	Adapter string
	BaseURL string
	Keys    []string
	Ctx     *int
}

// Providers returns the loaded provider slots.
func (c *Config) Providers() []ProviderSlot { return c.providers }

// Registry builds a providers.Registry covering every loaded provider slot.
// Native adapters omit base_url, so the providers' defaults apply.
func (c *Config) Registry() (*providers.Registry, error) {
	reg := providers.NewRegistry(nil)
	for _, p := range c.providers {
		d, ok := providerDefinition(p)
		if !ok {
			return nil, fmt.Errorf("config: unknown adapter %q for provider %s", p.Adapter, p.Name)
		}
		if d.BaseURL == "" {
			switch d.Protocol {
			case providers.ProtocolAnthropic:
				d.BaseURL = providers.AnthropicBaseURL
			case providers.ProtocolGemini:
				d.BaseURL = providers.GeminiBaseURL
			default:
				d.BaseURL = providers.DefaultBaseURL
			}
		}
		if err := reg.Define(d); err != nil {
			return nil, fmt.Errorf("config: provider %s: %w", p.Name, err)
		}
	}
	return reg, nil
}

// Model looks up the first slot entry for a canonical model name.
func (c *Config) Model(name string) (*ModelEntry, bool) {
	for i := range c.ModelList {
		if c.ModelList[i].ModelName == name {
			return &c.ModelList[i], true
		}
	}
	return nil, false
}

// Slots returns every slot entry of a model, in declared priority order.
func (c *Config) Slots(modelName string) []ModelEntry {
	var out []ModelEntry
	for _, e := range c.ModelList {
		if e.ModelName == modelName {
			out = append(out, e)
		}
	}
	return out
}

// Keys returns the rotation pool for a slot, normalising the single-key case.
func (m *ModelEntry) Keys() []string {
	if len(m.ModelParams.APIKeys) > 0 {
		return m.ModelParams.APIKeys
	}
	if m.ModelParams.APIKey != "" {
		return []string{m.ModelParams.APIKey}
	}
	return nil
}

// ProviderSlug is the adapter selector for this slot, defaulting to "openai"
// when not set (legacy prefix-derived entries).
func (m *ModelEntry) ProviderSlug() string {
	if m.Provider != "" {
		return m.Provider
	}
	if i := strings.Index(m.ModelParams.Model, "/"); i > 0 {
		return m.ModelParams.Model[:i]
	}
	return "openai"
}
