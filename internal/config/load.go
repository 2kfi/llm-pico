package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/arkan/llm-pico/internal/keysenv"
	"github.com/arkan/llm-pico/internal/providers"
	"gopkg.in/yaml.v3"
)

// Load reads keys.env, provider.yaml and models.yaml from dir, validating
// cross-references. Every file is mandatory: a models.yaml key is the name
// callers present, and its 'model' field names the string sent upstream.
// The legacy aliases.yaml layout is rejected with a migration hint.
func Load(dir string) (*Config, error) {
	env, err := keysenv.Load(filepath.Join(dir, "keys.env"))
	if err != nil {
		return nil, err
	}
	if legacy := filepath.Join(dir, "aliases.yaml"); fileExists(legacy) {
		return nil, fmt.Errorf(
			"aliases.yaml is no longer supported: give the caller-facing name its own models.yaml entry " +
				"and point the 'model' field at the upstream model it serves")
	}
	slots, err := loadProviders(filepath.Join(dir, "provider.yaml"), env)
	if err != nil {
		return nil, err
	}
	models, err := loadModels(filepath.Join(dir, "models.yaml"), slots)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		RouterSettings: RouterSettings{
			NumRetries:   2,
			CooldownTime: 45,
			CircuitBreaker: CircuitBreakerSettings{
				Enabled:          true,
				FailureThreshold: 3,
				RecoveryTimeout:  30,
			},
		},
		DegradationMode: "normal",
		ProxyKeys:       env.ProxyKeys,
		providers:       slots,
	}
	for _, m := range models {
		cfg.ModelList = append(cfg.ModelList, m.slots...)
	}
	return cfg, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// adapterCaps maps an adapter type to what it can serve.
var adapterCaps = map[string]providers.Capability{
	"openai":        {Images: true, Embeddings: true, STT: true, TTS: true},
	"anthropic":     {Images: true},
	"gemini":        {Images: true, Embeddings: true},
	"openai-compat": {Images: true, Embeddings: true, STT: true, TTS: true},
}

// providerProtocol maps a file adapter name onto the providers.Registry
// protocol it becomes.
var providerProtocol = map[string]providers.Protocol{
	"openai":        providers.ProtocolOpenAI,
	"anthropic":     providers.ProtocolAnthropic,
	"gemini":        providers.ProtocolGemini,
	"openai-compat": providers.ProtocolOpenAI,
}

func providerDefinition(p ProviderSlot) (providers.Definition, bool) {
	proto, ok := providerProtocol[p.Adapter]
	if !ok {
		return providers.Definition{}, false
	}
	return providers.Definition{
		Slug:         p.Name,
		DisplayName:  p.Name,
		BaseURL:      p.BaseURL,
		Protocol:     proto,
		Capabilities: adapterCaps[p.Adapter],
	}, true
}

func kindCaps(k Kind) providers.Capability {
	switch k {
	case KindTTS:
		return providers.Capability{TTS: true}
	case KindSTT:
		return providers.Capability{STT: true}
	case KindEmbeddings:
		return providers.Capability{Embeddings: true}
	default:
		return providers.Capability{Images: true}
	}
}

// kindSupports reports whether an adapter's capability set covers a kind.
func kindSupports(k Kind, caps providers.Capability) bool {
	switch k {
	case KindTTS:
		return caps.TTS
	case KindSTT:
		return caps.STT
	case KindEmbeddings:
		return caps.Embeddings
	default:
		return true
	}
}

func loadProviders(path string, env keysenv.File) ([]ProviderSlot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("provider.yaml: %w", err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("provider.yaml: %w", err)
	}
	top := docValue(&root, "providers")
	if top == nil {
		return nil, fmt.Errorf("provider.yaml:%d: expected a top-level 'providers' mapping", lineOf(&root))
	}

	var out []ProviderSlot
	for _, entry := range entries(top) {
		slot := ProviderSlot{
			Name:    entry.key,
			Adapter: str(entry.value, "adapter"),
			BaseURL: str(entry.value, "base_url"),
			Ctx:     intField(entry.value, "ctx"),
		}
		if slot.Ctx != nil && *slot.Ctx <= 0 {
			return nil, fmt.Errorf("provider.yaml:%d: %s: ctx must be positive", entry.keyLine, slot.Name)
		}
		switch slot.Adapter {
		case "":
			return nil, fmt.Errorf("provider.yaml:%d: %s: 'adapter' is required", entry.keyLine, slot.Name)
		case "openai-compat":
			if slot.BaseURL == "" {
				return nil, fmt.Errorf("provider.yaml:%d: %s: openai-compat requires base_url", entry.keyLine, slot.Name)
			}
		default:
			if _, ok := providerProtocol[slot.Adapter]; !ok {
				return nil, fmt.Errorf("provider.yaml:%d: %s: unknown adapter %q", entry.keyLine, slot.Name, slot.Adapter)
			}
		}

		keyNames, err := keysField(node(entry.value, "keys"))
		if err != nil {
			return nil, fmt.Errorf("provider.yaml:%d: %s: keys: %v", entry.keyLine, slot.Name, err)
		}
		if len(keyNames) == 0 {
			return nil, fmt.Errorf("provider.yaml:%d: %s: 'keys' is required", entry.keyLine, slot.Name)
		}
		for _, name := range keyNames {
			value, ok := env.Keys[name]
			if !ok {
				return nil, fmt.Errorf("provider.yaml:%d: %s: key %q not found in keys.env", entry.keyLine, slot.Name, name)
			}
			slot.Keys = append(slot.Keys, value)
		}
		out = append(out, slot)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("provider.yaml: no providers configured")
	}
	return out, nil
}

// keysField accepts a single scalar (comma-joined key names) or a list.
func keysField(n *yaml.Node) ([]string, error) {
	if n == nil {
		return nil, nil
	}
	if n.Kind == yaml.ScalarNode {
		if n.Tag == "!!null" {
			return nil, nil
		}
		return commaList(n.Value), nil
	}
	if n.Kind == yaml.SequenceNode {
		var out []string
		for _, item := range n.Content {
			if item.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("expected a key name at line %d", item.Line)
			}
			if v := strings.TrimSpace(item.Value); v != "" {
				out = append(out, v)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("expected a scalar or list at line %d", n.Line)
}

// limitNames names the limit fields each kind accepts.
var limitNames = map[Kind][]string{
	KindChat:       {"rpm", "rpd", "tpm", "tpd"},
	KindEmbeddings: {"rpm", "rpd", "tpm", "tpd"},
	KindTTS:        {"rpm", "rpd", "cpm", "cpd", "daily-audio-sec"},
	KindSTT:        {"rpm", "rpd", "daily-audio-sec", "audio-min"},
}

// modelDef is one models.yaml entry before slot expansion.
type modelDef struct {
	name     string
	line     int
	kind     Kind
	provider []string
	limits   map[string]*int
	ctx      *int
	dims     *int
	upstream string
	slots    []ModelEntry
}

func loadModels(path string, slots []ProviderSlot) ([]modelDef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("models.yaml: %w", err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("models.yaml: %w", err)
	}
	body := &root
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return nil, fmt.Errorf("models.yaml: file is empty")
		}
		body = root.Content[0]
	}
	if body.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("models.yaml:%d: expected a mapping of model names", body.Line)
	}

	byName := map[string]ProviderSlot{}
	for _, p := range slots {
		byName[p.Name] = p
	}

	var out []modelDef
	for _, entry := range entries(body) {
		if entry.value.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("models.yaml:%d: %s: expected a mapping", entry.value.Line, entry.key)
		}
		md, err := parseModel(entry, byName)
		if err != nil {
			return nil, err
		}
		out = append(out, md)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models.yaml: no models configured")
	}
	return out, nil
}

func parseModel(entry mapEntry, byName map[string]ProviderSlot) (modelDef, error) {
	md := modelDef{
		name:     entry.key,
		line:     entry.keyLine,
		limits:   map[string]*int{},
		upstream: entry.key,
	}

	for _, f := range entries(entry.value) {
		key, node := f.key, f.value
		switch key {
		case "kind":
			k := Kind(node.Value)
			if _, ok := limitNames[k]; !ok {
				return md, fmt.Errorf("models.yaml:%d: %s: unknown kind %q", f.keyLine, md.name, node.Value)
			}
			md.kind = k
		case "providers":
			names, err := nameList(node)
			if err != nil {
				return md, fmt.Errorf("models.yaml:%d: %s: providers: %v", f.keyLine, md.name, err)
			}
			if len(names) == 0 {
				return md, fmt.Errorf("models.yaml:%d: %s: providers must list at least one provider", f.keyLine, md.name)
			}
			for _, n := range names {
				if _, ok := byName[n]; !ok {
					return md, fmt.Errorf("models.yaml:%d: %s: unknown provider %q", f.keyLine, md.name, n)
				}
			}
			md.provider = names
		case "ctx":
			v, err := positiveInt(node, md.name, "ctx")
			if err != nil {
				return md, err
			}
			md.ctx = v
		case "dims":
			v, err := positiveInt(node, md.name, "dims")
			if err != nil {
				return md, err
			}
			md.dims = v
		case "model":
			// The caller-facing key is a name of the operator's choosing;
			// 'model' is what actually travels upstream.
			if strings.TrimSpace(node.Value) == "" {
				return md, fmt.Errorf("models.yaml:%d: %s: model must not be empty", f.keyLine, md.name)
			}
			md.upstream = strings.TrimSpace(node.Value)
		case "upstream":
			return md, fmt.Errorf("models.yaml:%d: %s: field 'upstream' was renamed to 'model'", f.keyLine, md.name)
		default:
			n, err := maybeLimit(md, f)
			if err != nil {
				return md, err
			}
			md.limits[key] = n
		}
	}

	if md.kind == "" {
		return md, fmt.Errorf("models.yaml:%d: %s: 'kind' is required", md.line, md.name)
	}
	return expandSlots(md, byName)
}

// maybeLimit validates a field outside the structural set as a per-kind limit.
func maybeLimit(md modelDef, f mapEntry) (*int, error) {
	for _, name := range limitNames[md.kind] {
		if name == f.key {
			v, err := positiveInt(f.value, md.name, f.key)
			if err != nil {
				return nil, err
			}
			return v, nil
		}
	}
	for _, kind := range []Kind{KindChat, KindEmbeddings, KindTTS, KindSTT} {
		if kind == md.kind {
			continue
		}
		for _, name := range limitNames[kind] {
			if name == f.key {
				return nil, fmt.Errorf("models.yaml:%d: %s: limit %q does not apply to kind %q",
					f.keyLine, md.name, f.key, md.kind)
			}
		}
	}
	return nil, fmt.Errorf("models.yaml:%d: %s: unknown field %q", f.keyLine, md.name, f.key)
}

func expandSlots(md modelDef, byName map[string]ProviderSlot) (modelDef, error) {
	for _, name := range md.provider {
		slot := byName[name]
		caps := adapterCaps[slot.Adapter]
		if !kindSupports(md.kind, caps) {
			return md, fmt.Errorf("models.yaml:%d: %s: provider %s (adapter %s) does not support kind %q",
				md.line, md.name, name, slot.Adapter, md.kind)
		}
		sl := ModelEntry{
			ModelName:      md.name,
			Kind:           md.kind,
			Provider:       name,
			Ctx:            md.ctx,
			CtxOverride:    slot.Ctx,
			Dims:           md.dims,
			ModelParams:    ModelParams{Model: md.upstream, APIKeys: slot.Keys, APIBase: slot.BaseURL},
			Images:         md.kind == KindChat && caps.Images,
			Embeddings:     md.kind == KindEmbeddings,
			STT:            md.kind == KindSTT,
			TTS:            md.kind == KindTTS,
			RPM:            md.limits["rpm"],
			RPD:            md.limits["rpd"],
			TPM:            md.limits["tpm"],
			TPD:            md.limits["tpd"],
			CPM:            md.limits["cpm"],
			CPD:            md.limits["cpd"],
			AudioSecPerDay: md.limits["daily-audio-sec"],
			AudioMin:       md.limits["audio-min"],
		}
		md.slots = append(md.slots, sl)
	}
	return md, nil
}

type mapEntry struct {
	key     string
	keyLine int
	value   *yaml.Node
}

// entries walks a mapping node as key/value pairs.
func entries(n *yaml.Node) []mapEntry {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	out := make([]mapEntry, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, mapEntry{
			key:     n.Content[i].Value,
			keyLine: n.Content[i].Line,
			value:   n.Content[i+1],
		})
	}
	return out
}

// node returns the value node for name in a mapping, or nil.
func node(n *yaml.Node, name string) *yaml.Node {
	for _, e := range entries(n) {
		if e.key == name {
			return e.value
		}
	}
	return nil
}

func str(n *yaml.Node, name string) string {
	if v := node(n, name); v != nil && v.Kind == yaml.ScalarNode {
		return strings.TrimSpace(v.Value)
	}
	return ""
}

// intField reads an integer scalar field; nil means absent or invalid.
func intField(n *yaml.Node, name string) *int {
	v := node(n, name)
	if v == nil || v.Kind != yaml.ScalarNode {
		return nil
	}
	var i int
	if err := v.Decode(&i); err != nil {
		return nil
	}
	return &i
}

// docValue returns the value node for a top-level mapping key.
func docValue(n *yaml.Node, name string) *yaml.Node {
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	return node(n, name)
}

// lineOf returns the first meaningful line of a node.
func lineOf(n *yaml.Node) int {
	if n == nil {
		return 0
	}
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return n.Content[0].Line
	}
	return n.Line
}

func positiveInt(node *yaml.Node, model, field string) (*int, error) {
	var v int
	if err := node.Decode(&v); err != nil {
		return nil, fmt.Errorf("models.yaml:%d: %s: %s must be an integer", node.Line, model, field)
	}
	if v <= 0 {
		return nil, fmt.Errorf("models.yaml:%d: %s: %s must be positive", node.Line, model, field)
	}
	return &v, nil
}

// nameList accepts both a flow/block list and a comma-joined scalar.
func nameList(n *yaml.Node) ([]string, error) {
	switch n.Kind {
	case yaml.SequenceNode:
		var out []string
		for _, item := range n.Content {
			if item.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("expected a provider name at line %d", item.Line)
			}
			if v := strings.TrimSpace(item.Value); v != "" {
				out = append(out, v)
			}
		}
		return out, nil
	case yaml.ScalarNode:
		return commaList(n.Value), nil
	default:
		return nil, fmt.Errorf("expected a list at line %d", n.Line)
	}
}

// commaList splits a scalar list on commas.
func commaList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
