package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arkan/llm-pico/internal/providers"
)

func keysEnv(providers ...string) string {
	lines := []string{"PROXY_KEYS=sk-house-1,sk-house-2"}
	for _, p := range providers {
		lines = append(lines, strings.ToUpper(p)+"_1_KEY=key-"+p)
	}
	return strings.Join(lines, "\n") + "\n"
}

func providerYAML() string {
	return `
providers:
  openai-1:
    adapter: openai
    keys: OPENAI_1_KEY
  anthropic-1:
    adapter: anthropic
    keys: ANTHROPIC_1_KEY
  google-1:
    adapter: gemini
    keys: GOOGLE_1_KEY
  relay-1:
    adapter: openai-compat
    base_url: https://relay.example.net/v1
    keys: RELAY_1_KEY
  gpu-1:
    adapter: openai-compat
    base_url: http://127.0.0.1:8080/v1
    keys: GPU_1_KEY
    ctx: 8192
`
}

func modelsYAML() string {
	return `
gemini-flash:
  kind: chat
  providers: google-1, relay-1
  ctx: 1000000
  rpm: 60
  rpd: 1500
  tpm: 200000
  tpd: 2000000

gpt-mini:
  kind: chat
  providers: openai-1
  upstream: gpt-4o-mini
  ctx: 128000
  rpm: 300

embedding-small:
  kind: embeddings
  providers: openai-1, google-1
  dims: 1536
  tpm: 1000000

tts-1:
  kind: tts
  providers: openai-1
  rpm: 30
  cpm: 12000
  daily-audio-sec: 7200

whisper-1:
  kind: stt
  providers: openai-1, gpu-1
  rpm: 20
  audio-min: 60
`
}

func aliasesYAML() string {
	return "cheap: gpt-mini\nsmol: gemini-flash\n"
}

func writeDir(t *testing.T, keys, providers, models, aliases string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("keys.env", keys)
	write("provider.yaml", providers)
	write("models.yaml", models)
	if aliases != "" {
		write("aliases.yaml", aliases)
	}
	return dir
}

func fullDir(t *testing.T) string {
	t.Helper()
	return writeDir(t,
		keysEnv("openai", "anthropic", "google", "relay", "gpu"),
		providerYAML(), modelsYAML(), aliasesYAML())
}

func TestLoadEndToEnd(t *testing.T) {
	cfg, err := Load(fullDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ProxyKeys) != 2 || cfg.ProxyKeys[0] != "sk-house-1" {
		t.Errorf("ProxyKeys = %v", cfg.ProxyKeys)
	}

	if got := len(cfg.ModelList); got != 8 {
		// gemini-flash x2, gpt-mini, embedding-small x2, tts-1, whisper-1 x2
		t.Fatalf("model entries = %d, want 8", got)
	}

	slots := cfg.Slots("gemini-flash")
	if len(slots) != 2 || slots[0].Provider != "google-1" || slots[1].Provider != "relay-1" {
		t.Fatalf("gemini-flash slots = %+v", slots)
	}
	if ctx := slots[0].EffectiveCtx(); ctx == nil || *ctx != 1000000 {
		t.Errorf("EffectiveCtx = %v", ctx)
	}
	if !slots[0].Images {
		t.Error("gemini chat slot should support images")
	}
	if !slots[1].Images {
		t.Error("openai-compat slot should claim images for chat")
	}

	whisper := cfg.Slots("whisper-1")
	if whisper[1].CtxOverride == nil || *whisper[1].CtxOverride != 8192 {
		t.Errorf("gpu slot ctx override = %v", whisper[1].CtxOverride)
	}
	if whisper[1].ModelParams.APIBase != "http://127.0.0.1:8080/v1" {
		t.Errorf("gpu slot base = %q", whisper[1].ModelParams.APIBase)
	}
	if !whisper[1].STT {
		t.Error("whisper gpu slot should be STT")
	}

	upstreamEntry, ok := cfg.Model("gpt-mini")
	if !ok || upstreamEntry.ModelParams.Model != "gpt-4o-mini" {
		t.Errorf("upstream override lookup failed: %v %v", upstreamEntry, ok)
	}

	if cfg.Aliases["cheap"] != "gpt-mini" || cfg.Canonical("cheap") != "gpt-mini" {
		t.Errorf("aliases = %v", cfg.Aliases)
	}
	if cfg.Canonical("gpt-mini") != "gpt-mini" {
		t.Error("canonical name must resolve to itself")
	}

	tts := cfg.Slots("tts-1")[0]
	if tts.CPM == nil || *tts.CPM != 12000 || tts.TPM != nil {
		t.Errorf("tts limits: %+v", tts)
	}
}

func TestRegistryBuiltFromProviders(t *testing.T) {
	cfg, err := Load(fullDir(t))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := cfg.Registry()
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"openai-1", "anthropic-1", "google-1", "relay-1", "gpu-1"} {
		spec, err := reg.Lookup(slug)
		if err != nil {
			t.Fatalf("lookup %s: %v", slug, err)
		}
		_ = spec
	}
	spec, _ := reg.Lookup("relay-1")
	if spec.Protocol != providers.ProtocolOpenAI || spec.BaseURL != "https://relay.example.net/v1" {
		t.Errorf("relay spec = %+v", spec)
	}
	anthropic, _ := reg.Lookup("anthropic-1")
	if anthropic.Protocol != providers.ProtocolAnthropic {
		t.Errorf("anthropic protocol = %s", anthropic.Protocol)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		keys    string
		prov    string
		models  string
		aliases string
		want    string
	}{
		{"unknown adapter", keysEnv("openai"),
			"providers:\n  bad:\n    adapter: wat\n    keys: OPENAI_1_KEY\n", "a:\n  kind: chat\n  providers: bad\n", "",
			"unknown adapter"},
		{"missing key ref", keysEnv("openai"),
			"providers:\n  openai-1:\n    adapter: openai\n    keys: NOPE_1_KEY\n", "a:\n  kind: chat\n  providers: openai-1\n", "",
			"not found in keys.env"},
		{"unknown provider ref", keysEnv("openai"),
			"providers:\n  openai-1:\n    adapter: openai\n    keys: OPENAI_1_KEY\n", "a:\n  kind: chat\n  providers: ghost\n", "",
			"unknown provider"},
		{"kind not supported", keysEnv("openai", "anthropic"),
			"providers:\n  anthropic-1:\n    adapter: anthropic\n    keys: ANTHROPIC_1_KEY\n",
			"v:\n  kind: stt\n  providers: anthropic-1\n", "",
			"does not support kind"},
		{"non-positive limit", keysEnv("openai"),
			"providers:\n  openai-1:\n    adapter: openai\n    keys: OPENAI_1_KEY\n",
			"a:\n  kind: chat\n  providers: openai-1\n  rpm: 0\n", "",
			"must be positive"},
		{"limit not for kind", keysEnv("openai"),
			"providers:\n  openai-1:\n    adapter: openai\n    keys: OPENAI_1_KEY\n",
			"a:\n  kind: chat\n  providers: openai-1\n  cpm: 100\n", "",
			"does not apply to kind"},
		{"unknown field", keysEnv("openai"),
			"providers:\n  openai-1:\n    adapter: openai\n    keys: OPENAI_1_KEY\n",
			"a:\n  kind: chat\n  providers: openai-1\n  typo: 1\n", "",
			"unknown field"},
		{"alias chain", keysEnv("openai"),
			"providers:\n  openai-1:\n    adapter: openai\n    keys: OPENAI_1_KEY\n",
			"a:\n  kind: chat\n  providers: openai-1\n", "b: a\nc: b\n",
			"alias chains"},
		{"missing keys", keysEnv(),
			"providers:\n  openai-1:\n    adapter: openai\n    keys: OPENAI_1_KEY\n",
			"a:\n  kind: chat\n  providers: openai-1\n", "",
			"not found in keys.env"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeDir(t, tc.keys, tc.prov, tc.models, tc.aliases))
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestLoadErrorsCarryPathAndLine(t *testing.T) {
	_, err := Load(writeDir(t, keysEnv("openai"),
		"providers:\n  openai-1:\n    adapter: openai\n    keys: OPENAI_1_KEY\n",
		"a:\n  kind: wob\n", ""))
	if err == nil || !strings.Contains(err.Error(), "models.yaml") {
		t.Fatalf("error = %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "models.yaml:2") {
		t.Fatalf("error should carry a line number, got %v", err)
	}
}

func TestLoadWithoutAliasesFile(t *testing.T) {
	cfg, err := Load(writeDir(t, keysEnv("openai"),
		"providers:\n  openai-1:\n    adapter: openai\n    keys: OPENAI_1_KEY\n",
		"a:\n  kind: chat\n  providers: openai-1\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Aliases) != 0 {
		t.Errorf("aliases = %v", cfg.Aliases)
	}
}
