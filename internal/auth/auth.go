// Package auth authenticates API keys and authorises requests.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// HashKey returns the SHA-256 hash of a raw key.
func HashKey(rawKey string) string {
	sum := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(sum[:])
}

// PrefixFromKey renders a displayable, non-secret portion of a key.
func PrefixFromKey(rawKey string) string {
	if len(rawKey) > 12 {
		return rawKey[:12] + "..."
	}
	return rawKey
}

// ExtractBearer returns the token from an Authorization header, or "".
func ExtractBearer(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// Role distinguishes the kinds of caller the proxy accepts.
type Role string

const RoleUser Role = "user"

// Identity is the authenticated caller, resolved once per request.
type Identity struct {
	Role      Role
	KeyHash   string
	KeyPrefix string
}

// VerifyHMAC checks an HMAC-SHA256 signature over the raw body. Clients send
// "sha256=<hex>".
func VerifyHMAC(secret string, body []byte, signature string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return secureEqual(expected, signature)
}

// secureEqual compares two strings without leaking length or content by timing.
func secureEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func nowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05") }

// AuditLog appends structured JSONL records for administrative actions. It is
// best-effort: failing to audit must never fail the action being audited.
type AuditLog struct {
	path string
}

// NewAuditLog creates an audit log writing to path.
func NewAuditLog(path string) *AuditLog { return &AuditLog{path: path} }

// Write appends one entry.
func (a *AuditLog) Write(entry map[string]any) {
	if a == nil || a.path == "" {
		return
	}
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()

	if _, ok := entry["ts"]; !ok {
		entry["ts"] = nowISO()
	}
	// HTML escaping is disabled so URLs and model names survive intact.
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(entry)
}
