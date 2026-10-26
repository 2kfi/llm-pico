package config

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed defaults/provider.yaml defaults/models.yaml defaults/aliases.yaml
var defaultFS embed.FS

// defaultFiles are written by Init, in the names Load requires. keys.env is
// generated rather than embedded so every install gets a fresh PROXY_KEYS.
var defaultFiles = []string{"provider.yaml", "models.yaml", "aliases.yaml"}

// configFiles names every starter file, the union an init sweep looks for.
var configFiles = append([]string{"keys.env"}, defaultFiles...)

// GenerateProxyKey returns a fresh bearer key clients can present to the proxy.
func GenerateProxyKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("proxy key generation: %w", err)
	}
	return "sk-" + hex.EncodeToString(b), nil
}

// keysEnvTemplate is the starter keys.env. The upstream key is a placeholder
// on purpose: it passes the non-empty parse rule but fails at every upstream
// call until it is replaced, so a forgotten key is loud instead of silent.
const keysEnvTemplate = `# llm-pico starter keys. Created by 'llm-pico --init'.
# PROXY_KEYS is the client credential: whoever presents a key from this list
# can spend your upstream API keys. Copy it to every caller you authorize.
PROXY_KEYS=%s

# Upstream credential. Format: PROVIDER[_SLOT]_KEY, whose name is what the
# 'keys:' lines in provider.yaml reference. Replace the placeholder below
# with your OpenAI API key; until you do, every request will fail upstream.
OPENAI_MAIN_KEY=sk-paste-your-openai-key-here
`

// Init writes a starter configuration into dir: three embedded defaults plus
// a keys.env carrying a freshly generated PROXY_KEYS. An empty directory is
// created if needed; an existing configuration is never overwritten, so
// running -init twice cannot lose edits.
//
// It returns the generated PROXY_KEYS value, which the caller should show the
// user exactly once.
func Init(dir string) (string, error) {
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create config dir: %w", err)
		}
	}

	var existing []string
	for _, name := range configFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			existing = append(existing, name)
		}
	}
	if len(existing) > 0 {
		return "", fmt.Errorf("refusing to overwrite an existing configuration: %v already present in %s", existing, dir)
	}

	proxyKey, err := GenerateProxyKey()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "keys.env"), fmt.Appendf(nil, keysEnvTemplate, proxyKey), 0o600); err != nil {
		return "", fmt.Errorf("write keys.env: %w", err)
	}
	for _, name := range defaultFiles {
		content, err := fs.ReadFile(defaultFS, "defaults/"+name)
		if err != nil {
			return "", fmt.Errorf("read embedded default %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			return "", fmt.Errorf("write %s: %w", name, err)
		}
	}
	return proxyKey, nil
}

// IsEmptyConfigDir reports whether dir holds none of the files llm-pico's
// configuration needs. Auto-initialisation fires only then, so a directory
// with a partially written configuration is never silently completed with
// generated defaults.
func IsEmptyConfigDir(dir string) bool {
	for _, name := range configFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return false
		}
	}
	return true
}

// NeedsInit reports whether Load failed because the configuration directory
// is being used for the first time: keys.env, the one file Load must have,
// does not exist. In that case the user is on a freshly downloaded binary
// with no examples/ to copy, and the error should point at --init.
func NeedsInit(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "keys.env"))
	return err != nil
}
