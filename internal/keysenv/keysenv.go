// Package keysenv parses keys.env: the client bearer keys accepted by the
// proxy and the upstream credentials referenced by provider.yaml.
package keysenv

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// File is one parsed keys.env.
type File struct {
	// ProxyKeys is the raw client bearer key list from PROXY_KEYS.
	ProxyKeys []string
	// Keys maps upstream key names (e.g. OPENAI_1_KEY) to their values.
	Keys map[string]string
}

// upstreamName matches PROVIDER_KEY or PROVIDER_SLOT_KEY in upper snake case.
var upstreamName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*_KEY$`)

// Load reads and parses the file at path.
func Load(path string) (File, error) {
	f, err := os.Open(path)
	if err != nil {
		return File{}, fmt.Errorf("keysenv: %w", err)
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads key=value lines. Full-line comments start with #; a value may
// carry a trailing comment after " #".
func Parse(r io.Reader) (File, error) {
	out := File{Keys: map[string]string{}}
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if i := strings.Index(text, " #"); i >= 0 {
			text = strings.TrimSpace(text[:i])
		}
		name, value, ok := strings.Cut(text, "=")
		if !ok {
			return File{}, fmt.Errorf("keysenv:%d: expected KEY=value, got %q", line, text)
		}
		name = strings.TrimSpace(name)
		value = unquote(strings.TrimSpace(value))
		if value == "" {
			return File{}, fmt.Errorf("keysenv:%d: empty value for %s", line, name)
		}

		switch {
		case name == "PROXY_KEYS":
			out.ProxyKeys = splitList(value)
		case upstreamName.MatchString(name):
			out.Keys[name] = value
		case name == "http_proxy" || name == "https_proxy" || name == "no_proxy":
			// Environment proxies pass through untouched.
		default:
			// Unknown vars are ignored rather than fatal: keys.env is often
			// sourced next to unrelated secrets.
			fmt.Fprintf(os.Stderr, "keysenv:%d: ignoring %s (not PROXY_KEYS or PROVIDER_KEY)\n", line, name)
		}
	}
	if err := sc.Err(); err != nil {
		return File{}, fmt.Errorf("keysenv: read: %w", err)
	}
	if len(out.ProxyKeys) == 0 && len(out.Keys) == 0 {
		return File{}, fmt.Errorf("keysenv: no PROXY_KEYS or *_KEY entries found")
	}

	// Secure default: with no PROXY_KEYS the proxy accepts no client key at
	// all rather than any bearer token.
	if len(out.ProxyKeys) == 0 {
		fmt.Fprintln(os.Stderr, "keysenv: PROXY_KEYS is not set; every client request will be rejected until it is configured")
	}
	return out, nil
}

// splitList splits a comma-separated list and drops empty entries.
func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// unquote strips one layer of matching quotes.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}
