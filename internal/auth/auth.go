// Package auth authenticates API keys and authorises requests.
//
// Three credential types exist: the master key (instance administrator), user
// keys (owned by a user, who belongs to a team), and per-key scopes. Effective
// limits are the most restrictive of key, user and team, so issuing a key can
// never widen a limit.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/arkan/llm-pico/internal/org"
	"github.com/arkan/llm-pico/internal/ratelimit"
	"github.com/arkan/llm-pico/internal/store"
)

// HashKey returns the SHA-256 hash of a raw key. Only hashes are persisted, so
// a database disclosure does not leak usable credentials.
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

// Errors returned when a credential fails to authenticate.
var (
	ErrInvalidKey   = errors.New("auth: invalid API key")
	ErrIPNotAllowed = errors.New("auth: IP not allowed")
	ErrMissingSig   = errors.New("auth: missing X-Signature header")
	ErrInvalidSig   = errors.New("auth: invalid HMAC signature")
)

// Role distinguishes an administrator from a scoped user key.
type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

// Scope is a named permission checked by admin endpoints.
type Scope string

const (
	ScopeRead  Scope = "read"
	ScopeWrite Scope = "write"
	ScopeAdmin Scope = "admin"
)

// Identity is the authenticated caller, resolved once per request.
type Identity struct {
	Role        Role
	KeyHash     string // empty for the master key
	KeyPrefix   string
	UserID      *int64
	Allowlist   []string // nil means unrestricted
	Scopes      []Scope
	IPAllowlist []string
	Limits      org.Limits
}

// IsAdmin reports whether the caller may use the admin API.
func (i *Identity) IsAdmin() bool { return i.Role == RoleAdmin }

// CanAccessModel reports whether the identity may use modelName.
func (i *Identity) CanAccessModel(modelName string) bool {
	if i.Allowlist == nil {
		return true
	}
	for _, m := range i.Allowlist {
		if m == modelName {
			return true
		}
	}
	return false
}

// HasScope reports whether the identity carries s. Admins hold every scope.
func (i *Identity) HasScope(s Scope) bool {
	if i.Role == RoleAdmin {
		return true
	}
	for _, have := range i.Scopes {
		if have == s {
			return true
		}
	}
	return false
}

// Ledger identifies the row usage should be attributed to.
func (i *Identity) Ledger(masterKey string) (keyHash, keyPrefix string) {
	if i.Role == RoleAdmin {
		return masterKey, "master"
	}
	return i.KeyHash, i.KeyPrefix
}

// LimiterLimits renders the identity's user-level limits for the rate limiter.
func (i *Identity) LimiterLimits() ratelimit.Limits {
	return ratelimit.Limits{
		ratelimit.RPM: i.Limits.RPM,
		ratelimit.RPD: i.Limits.RPD,
		ratelimit.TPM: i.Limits.TPM,
		ratelimit.TPD: i.Limits.TPD,
		ratelimit.ASH: i.Limits.ASH,
		ratelimit.ASD: i.Limits.ASD,
	}
}

// Verifier resolves raw bearer tokens into identities.
type Verifier struct {
	db *store.Store
}

// NewVerifier creates a Verifier backed by db.
func NewVerifier(db *store.Store) *Verifier { return &Verifier{db: db} }

// Verify authenticates rawKey against the master key and the key table.
//
// The master key is accepted in either raw or hashed form: the dashboard
// stores a SHA-256 hash so the raw key never transits the network, while the
// CLI passes the raw value. Both are supported so an existing deployment keeps
// working after the port.
func (v *Verifier) Verify(ctx context.Context, rawKey, masterKey string) (*Identity, error) {
	if masterKey != "" {
		if secureEqual(rawKey, masterKey) || secureEqual(rawKey, HashKey(masterKey)) {
			return &Identity{
				Role:      RoleAdmin,
				KeyPrefix: "master",
				Scopes:    []Scope{ScopeRead, ScopeWrite, ScopeAdmin},
			}, nil
		}
	}

	keyHash := HashKey(rawKey)
	row, err := v.db.QueryRow(ctx, `
		SELECT id, key_prefix, expires_at, model_allowlist, ip_allowlist, user_id,
		       rpm_limit, rpd_limit, tpm_limit, tpd_limit, ash_limit, asd_limit
		FROM user_keys WHERE key_hash = ? AND is_active = 1`, keyHash)
	if err != nil {
		return nil, err
	}

	var (
		keyID       int64
		prefix      string
		expiresAt   *string
		allowlistJS *string
		ipAllowJS   *string
		userID      *int64
		limits      org.Limits
	)
	if err := row.Scan(&keyID, &prefix, &expiresAt, &allowlistJS, &ipAllowJS, &userID,
		&limits.RPM, &limits.RPD, &limits.TPM, &limits.TPD, &limits.ASH, &limits.ASD); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		// Any scan failure here means no usable row matched.
		return nil, ErrInvalidKey
	}

	now := time.Now().UTC().Format("2006-01-02T15:04:05")
	if expiresAt != nil && *expiresAt != "" && *expiresAt < now {
		return nil, ErrInvalidKey
	}

	id := &Identity{
		Role:        RoleUser,
		KeyHash:     keyHash,
		KeyPrefix:   prefix,
		UserID:      userID,
		IPAllowlist: decodeList(ipAllowJS),
		Limits:      limits,
		Allowlist:   decodeList(allowlistJS),
	}

	// Inherit from the owning user and team, taking the most restrictive value
	// at each window and intersecting allowlists.
	if userID != nil {
		user, team, err := org.Resolve(ctx, v.db, *userID)
		if err != nil {
			return nil, err
		}
		if user == nil || !user.IsActive {
			return nil, ErrInvalidKey
		}
		if team != nil && !team.IsActive {
			return nil, ErrInvalidKey
		}
		id.Limits = id.Limits.Merge(user.Limits)
		if team != nil {
			id.Limits = id.Limits.Merge(team.Limits)
		}
		id.Allowlist = intersectAllowlists(
			id.Allowlist, listOf(user), listOf(team),
		)
	}

	scopes, err := v.scopesFor(ctx, keyID)
	if err != nil {
		return nil, err
	}
	id.Scopes = scopes
	return id, nil
}

func listOf(e any) []string {
	switch t := e.(type) {
	case *org.User:
		if t != nil {
			return t.Allowlist
		}
	case *org.Team:
		if t != nil {
			return t.Allowlist
		}
	}
	return nil
}

func (v *Verifier) scopesFor(ctx context.Context, keyID int64) ([]Scope, error) {
	rows, err := v.db.Query(ctx, `SELECT scope FROM api_key_scopes WHERE key_id = ?`, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Scope
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, Scope(s))
	}
	return out, rows.Err()
}

// SetScopes replaces the scope set attached to a key.
func SetScopes(ctx context.Context, db *store.Store, keyID int64, scopes []string) error {
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM api_key_scopes WHERE key_id = ?`, keyID); err != nil {
			return err
		}
		for _, s := range scopes {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO api_key_scopes (key_id, scope) VALUES (?, ?)`, keyID, s); err != nil {
				return err
			}
		}
		return nil
	})
}

// CheckIP reports whether clientIP falls inside any CIDR in allowlist. An empty
// allowlist permits every address.
func CheckIP(clientIP string, allowlist []string) bool {
	if len(allowlist) == 0 {
		return true
	}
	addr := net.ParseIP(clientIP)
	if addr == nil {
		// An unparseable address cannot be shown to be inside any range.
		return false
	}
	for _, cidr := range allowlist {
		if _, network, err := net.ParseCIDR(cidr); err == nil {
			if network.Contains(addr) {
				return true
			}
			continue
		}
		// A bare address is accepted as a single-host range.
		if other := net.ParseIP(cidr); other != nil && other.Equal(addr) {
			return true
		}
	}
	return false
}

// VerifyHMAC checks an HMAC-SHA256 signature over the raw body. Clients send
// "sha256=<hex>".
func VerifyHMAC(secret string, body []byte, signature string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return secureEqual(expected, signature)
}

// ClientIP extracts the caller address. X-Forwarded-For is honoured only when
// TrustProxy is set, because any client can forge the header and otherwise
// walk straight through an IP allowlist.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, found := strings.Cut(xff, ","); found {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(xff)
		}
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// SecureEqual compares two strings without leaking length or content by
// timing. It is exported so the admin API can compare master-key credentials
// the same way the auth path does.
func SecureEqual(a, b string) bool { return secureEqual(a, b) }

// secureEqual compares two strings without leaking length or content by timing.
func secureEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

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
		entry["ts"] = store.NowISO()
	}
	// HTML escaping is disabled so URLs and model names survive intact.
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(entry)
}

// --- helpers ---

func decodeList(s *string) []string {
	if s == nil || *s == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(*s), &out); err != nil {
		return nil
	}
	return out
}

// intersectAllowlists intersects model allowlists across key, user and team.
// An absent list means unrestricted; the result is the intersection of those
// present, and an empty intersection denies everything.
func intersectAllowlists(lists ...[]string) []string {
	var present [][]string
	for _, l := range lists {
		if l == nil {
			continue
		}
		present = append(present, l)
	}
	if len(present) == 0 {
		return nil
	}
	allowed := map[string]bool{}
	for i, m := range present[0] {
		allowed[m] = i < len(present[0])
	}
	for _, l := range present[1:] {
		set := map[string]bool{}
		for _, m := range l {
			set[m] = true
		}
		for m := range allowed {
			if !set[m] {
				allowed[m] = false
			}
		}
	}
	out := make([]string, 0, len(allowed))
	for m, ok := range allowed {
		if ok {
			out = append(out, m)
		}
	}
	return out
}
