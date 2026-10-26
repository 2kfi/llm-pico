package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arkan/llm-pico/internal/keysenv"
)

// -init must produce a configuration Load accepts, with a real PROXY_KEYS
// and a loud upstream placeholder; a second run must refuse rather than
// overwrite whatever the user edited.
func TestInitWritesLoadableConfig(t *testing.T) {
	dir := t.TempDir()

	key, err := Init(dir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.HasPrefix(key, "sk-") || len(key) < 40 {
		t.Fatalf("generated key = %q, want an sk- key with real entropy", key)
	}

	env, err := keysenv.Load(filepath.Join(dir, "keys.env"))
	if err != nil {
		t.Fatalf("keys.env does not parse: %v", err)
	}
	if got := env.ProxyKeys; len(got) != 1 || got[0] != key {
		t.Fatalf("PROXY_KEYS = %v, want exactly the generated key %q", got, key)
	}
	if v, ok := env.Keys["OPENAI_MAIN_KEY"]; !ok || v == "" {
		t.Fatalf("OPENAI_MAIN_KEY = %q, present=%v, want a non-empty loud placeholder", v, ok)
	}

	if _, err := Load(dir); err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}

	// Point the placeholder at nothing the reload path would accept — the
	// point is only that re-init refuses to clobber.
	if err := os.WriteFile(filepath.Join(dir, "keys.env"),
		[]byte("PROXY_KEYS=sk-user-edited\nOPENAI_MAIN_KEY=sk-user-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(dir); err == nil {
		t.Fatal("second Init succeeded, want a refusal to overwrite")
	} else if !strings.Contains(err.Error(), "already present") {
		t.Fatalf("refusal error = %v, want an overwrite refusal", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "keys.env")); err != nil || !strings.Contains(string(b), "sk-user-edited") {
		t.Fatalf("second Init clobbered user edits: %q (%v)", b, err)
	}
}

// Auto-initialisation must fire only for a directory holding none of the
// config files: any file present means the user is mid-edit, and generated
// defaults must not fill the gaps.
func TestIsEmptyConfigDir(t *testing.T) {
	dir := t.TempDir()
	if !IsEmptyConfigDir(dir) {
		t.Fatal("empty dir reported as non-empty")
	}
	if err := os.WriteFile(filepath.Join(dir, "provider.yaml"), []byte("providers:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if IsEmptyConfigDir(dir) {
		t.Fatal("dir with provider.yaml reported as empty")
	}
}

// Nested directories are created on demand, and the flag also works when
// pointed at an existing empty directory.
func TestInitCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "config")
	if _, err := Init(dir); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
}
