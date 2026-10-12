package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/arkan/llm-pico/internal/org"
	"github.com/arkan/llm-pico/internal/store"
)

func newVerifier(t *testing.T) (*Verifier, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewVerifier(db), db
}

// seedKey inserts a user key row for the given raw key and returns its id.
// Options narrow the key; anything unset is left unlimited.
func seedKey(t *testing.T, db *store.Store, rawKey string, opts ...func(*keyOpts)) int64 {
	t.Helper()
	o := keyOpts{}
	for _, fn := range opts {
		fn(&o)
	}
	hash := HashKey(rawKey)
	_, err := db.Exec(context.Background(), `
		INSERT INTO user_keys
		  (key_hash, key_prefix, label, is_active, created_at, user_id, ip_allowlist,
		   expires_at, model_allowlist, rpm_limit, tpm_limit)
		VALUES (?, ?, 'test', 1, ?, ?, ?, ?, ?, ?, ?)`,
		hash, PrefixFromKey(rawKey), store.Now(), o.userID, o.ipAllowlist,
		o.expiresAt, o.allowlist, o.rpm, o.tpm)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.Get(context.Background(), &id, `SELECT id FROM user_keys WHERE key_hash = ?`, hash); err != nil {
		t.Fatal(err)
	}
	return id
}

type keyOpts struct {
	userID      *int64
	ipAllowlist *string
	expiresAt   *string
	allowlist   *string
	rpm         *int64
	tpm         *int64
}

func withUser(id int64) func(*keyOpts)   { return func(o *keyOpts) { o.userID = &id } }
func withIPs(s string) func(*keyOpts)    { return func(o *keyOpts) { o.ipAllowlist = &s } }
func withExpiry(s string) func(*keyOpts) { return func(o *keyOpts) { o.expiresAt = &s } }
func withAllow(s string) func(*keyOpts)  { return func(o *keyOpts) { o.allowlist = &s } }
func withRPM(n int64) func(*keyOpts)     { return func(o *keyOpts) { o.rpm = &n } }
func withTPM(n int64) func(*keyOpts)     { return func(o *keyOpts) { o.tpm = &n } }

// --- hashing ---

func TestHashKeyIsStableAndPrefixed(t *testing.T) {
	h := HashKey("sk-pico-abc")
	if len(h) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars", len(h))
	}
	if h != HashKey("sk-pico-abc") {
		t.Error("hash is not deterministic")
	}
	if HashKey("sk-pico-abc") == HashKey("sk-pico-abd") {
		t.Error("distinct keys produced the same hash")
	}
}

func TestPrefixFromKey(t *testing.T) {
	if got := PrefixFromKey("sk-pico-1234567890abcdef"); got != "sk-pico-1234..." {
		t.Errorf("got %q, want sk-pico-1234...", got)
	}
	if got := PrefixFromKey("short"); got != "short" {
		t.Errorf("got %q, want the key unchanged when shorter than 12 chars", got)
	}
}

func TestExtractBearer(t *testing.T) {
	cases := map[string]string{
		"Bearer abc123":    "abc123",
		"Bearer  abc123  ": "abc123",
		"bearer abc123":    "",
		"Basic abc123":     "",
		"":                 "",
	}
	for in, want := range cases {
		if got := ExtractBearer(in); got != want {
			t.Errorf("ExtractBearer(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- verification ---

func TestVerifyMasterKey(t *testing.T) {
	v, _ := newVerifier(t)
	id, err := v.Verify(context.Background(), "sk-pico-master", "sk-pico-master")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !id.IsAdmin() {
		t.Error("master key should resolve to an admin identity")
	}
	if id.KeyPrefix != "master" {
		t.Errorf("KeyPrefix = %q, want master", id.KeyPrefix)
	}
}

func TestVerifyMasterKeyAcceptsHashedForm(t *testing.T) {
	v, _ := newVerifier(t)
	// The dashboard sends sha256(rawKey) while the stored value is the raw
	// key; both must authenticate.
	hashed := HashKey("sk-pico-master")
	id, err := v.Verify(context.Background(), hashed, "sk-pico-master")
	if err != nil {
		t.Fatalf("Verify hashed master key: %v", err)
	}
	if !id.IsAdmin() {
		t.Error("hashed master key should resolve to an admin identity")
	}
}

func TestVerifyUserKey(t *testing.T) {
	v, db := newVerifier(t)
	seedKey(t, db, "sk-pico-user")

	id, err := v.Verify(context.Background(), "sk-pico-user", "")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.Role != RoleUser {
		t.Errorf("role = %s, want user", id.Role)
	}
	if want := HashKey("sk-pico-user"); id.KeyHash != want {
		t.Errorf("KeyHash = %q, want the stored hash %q", id.KeyHash, want)
	}
}

func TestVerifyRejectsUnknownKey(t *testing.T) {
	v, _ := newVerifier(t)
	if _, err := v.Verify(context.Background(), "sk-pico-nope", ""); err != ErrInvalidKey {
		t.Errorf("err = %v, want ErrInvalidKey", err)
	}
}

func TestVerifyRejectsRevokedKey(t *testing.T) {
	v, db := newVerifier(t)
	seedKey(t, db, "sk-pico-revoked")
	if _, err := db.Exec(context.Background(),
		`UPDATE user_keys SET is_active = 0 WHERE key_hash = ?`, HashKey("sk-pico-revoked")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background(), "sk-pico-revoked", ""); err != ErrInvalidKey {
		t.Errorf("err = %v, want ErrInvalidKey for a revoked key", err)
	}
}

func TestVerifyRejectsExpiredKey(t *testing.T) {
	v, db := newVerifier(t)
	seedKey(t, db, "sk-pico-expired", withExpiry("2000-01-01T00:00:00"))
	if _, err := v.Verify(context.Background(), "sk-pico-expired", ""); err != ErrInvalidKey {
		t.Errorf("err = %v, want ErrInvalidKey for an expired key", err)
	}
}

func TestVerifyAcceptsFutureExpiry(t *testing.T) {
	v, db := newVerifier(t)
	seedKey(t, db, "sk-pico-future", withExpiry("2999-01-01T00:00:00"))
	if _, err := v.Verify(context.Background(), "sk-pico-future", ""); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// --- scopes ---

func TestSetAndGetScopes(t *testing.T) {
	v, db := newVerifier(t)
	id := seedKey(t, db, "sk-pico-scope-test")

	if err := SetScopes(context.Background(), db, id, []string{"read", "write"}); err != nil {
		t.Fatal(err)
	}
	identity, err := v.Verify(context.Background(), "sk-pico-scope-test", "")
	if err != nil {
		t.Fatal(err)
	}
	if !identity.HasScope(ScopeRead) || !identity.HasScope(ScopeWrite) {
		t.Errorf("scopes = %v, want read and write", identity.Scopes)
	}
	if identity.HasScope(ScopeAdmin) {
		t.Error("identity should not hold the admin scope")
	}
}

func TestSetScopesReplaces(t *testing.T) {
	_, db := newVerifier(t)
	id := seedKey(t, db, "sk-pico-scope-replace")
	ctx := context.Background()

	if err := SetScopes(ctx, db, id, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := SetScopes(ctx, db, id, []string{"c"}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Get(ctx, &count, `SELECT COUNT(*) FROM api_key_scopes WHERE key_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("scope count = %d, want 1", count)
	}
}

func TestAdminHoldsEveryScope(t *testing.T) {
	v, _ := newVerifier(t)
	id, err := v.Verify(context.Background(), "sk-pico-master", "sk-pico-master")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []Scope{ScopeRead, ScopeWrite, ScopeAdmin} {
		if !id.HasScope(s) {
			t.Errorf("admin missing scope %s", s)
		}
	}
}

// --- model allowlist ---

func TestAllowlistNilMeansUnrestricted(t *testing.T) {
	v, db := newVerifier(t)
	seedKey(t, db, "sk-pico-open")
	id, err := v.Verify(context.Background(), "sk-pico-open", "")
	if err != nil {
		t.Fatal(err)
	}
	if !id.CanAccessModel("anything") {
		t.Error("a key with no allowlist should reach any model")
	}
}

func TestAllowlistRestricts(t *testing.T) {
	v, db := newVerifier(t)
	seedKey(t, db, "sk-pico-limited", withAllow(`["gpt-4","claude"]`))
	id, err := v.Verify(context.Background(), "sk-pico-limited", "")
	if err != nil {
		t.Fatal(err)
	}
	if !id.CanAccessModel("gpt-4") {
		t.Error("gpt-4 should be allowed")
	}
	if id.CanAccessModel("gpt-3.5-turbo") {
		t.Error("gpt-3.5-turbo should be denied")
	}
}

// --- limit inheritance ---

func TestLimitsAreMostRestrictive(t *testing.T) {
	v, db := newVerifier(t)
	ctx := context.Background()

	team, err := org.CreateTeam(ctx, db, "team-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	user, err := org.CreateUser(ctx, db, team.ID, "a@example.com", "A")
	if err != nil {
		t.Fatal(err)
	}
	// User allows 100 rpm, team allows 10, key sets 50. Tightest wins: 10.
	if _, err := org.UpdateUserLimits(ctx, db, user.ID, org.Limits{RPM: ptr(100)}); err != nil {
		t.Fatal(err)
	}
	if _, err := org.UpdateTeamLimits(ctx, db, team.ID, org.Limits{RPM: ptr(10)}); err != nil {
		t.Fatal(err)
	}
	seedKey(t, db, "sk-pico-limits", withUser(user.ID), withRPM(50))

	id, err := v.Verify(ctx, "sk-pico-limits", "")
	if err != nil {
		t.Fatal(err)
	}
	if id.Limits.RPM == nil || *id.Limits.RPM != 10 {
		t.Errorf("rpm = %v, want the team's tighter limit of 10", id.Limits.RPM)
	}
}

func TestUnlimitedAtOneLevelDoesNotConstrain(t *testing.T) {
	v, db := newVerifier(t)
	ctx := context.Background()

	team, _ := org.CreateTeam(ctx, db, "team-b", nil)
	user, _ := org.CreateUser(ctx, db, team.ID, "b@example.com", "B")
	// Only the key sets a limit; user and team are nil (unlimited).
	seedKey(t, db, "sk-pico-solo-limit", withUser(user.ID), withRPM(42))

	id, err := v.Verify(ctx, "sk-pico-solo-limit", "")
	if err != nil {
		t.Fatal(err)
	}
	if id.Limits.RPM == nil || *id.Limits.RPM != 42 {
		t.Errorf("rpm = %v, want the key's limit of 42", id.Limits.RPM)
	}
}

func TestAllowlistIntersectsAcrossKeyUserTeam(t *testing.T) {
	v, db := newVerifier(t)
	ctx := context.Background()

	team, _ := org.CreateTeam(ctx, db, "team-c", nil)
	user, _ := org.CreateUser(ctx, db, team.ID, "c@example.com", "C")
	if _, err := org.SetUserAllowlist(ctx, db, user.ID, []string{"a", "b", "c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := org.SetTeamAllowlist(ctx, db, team.ID, []string{"b", "c", "d"}); err != nil {
		t.Fatal(err)
	}
	seedKey(t, db, "sk-pico-intersect", withUser(user.ID), withAllow(`["a","b","c","d"]`))

	id, err := v.Verify(ctx, "sk-pico-intersect", "")
	if err != nil {
		t.Fatal(err)
	}
	// key∩user∩team = {b, c}
	if len(id.Allowlist) != 2 {
		t.Fatalf("allowlist = %v, want exactly [b c]", id.Allowlist)
	}
	if !id.CanAccessModel("b") || !id.CanAccessModel("c") {
		t.Errorf("allowlist = %v, want b and c", id.Allowlist)
	}
	if id.CanAccessModel("a") || id.CanAccessModel("d") {
		t.Errorf("allowlist = %v, a and d should be excluded", id.Allowlist)
	}
}

func TestInactiveUserRejectsKey(t *testing.T) {
	v, db := newVerifier(t)
	ctx := context.Background()

	team, _ := org.CreateTeam(ctx, db, "team-d", nil)
	user, _ := org.CreateUser(ctx, db, team.ID, "d@example.com", "D")
	seedKey(t, db, "sk-pico-inactive", withUser(user.ID))

	if _, err := db.Exec(ctx, `UPDATE users SET is_active = 0 WHERE id = ?`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(ctx, "sk-pico-inactive", ""); err != ErrInvalidKey {
		t.Errorf("err = %v, want ErrInvalidKey for a deactivated user", err)
	}
}

func TestDeactivatedTeamRejectsKey(t *testing.T) {
	v, db := newVerifier(t)
	ctx := context.Background()

	team, _ := org.CreateTeam(ctx, db, "team-e", nil)
	user, _ := org.CreateUser(ctx, db, team.ID, "e@example.com", "E")
	seedKey(t, db, "sk-pico-deactivated", withUser(user.ID))

	if _, err := org.DeactivateTeam(ctx, db, team.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(ctx, "sk-pico-deactivated", ""); err != ErrInvalidKey {
		t.Errorf("err = %v, want ErrInvalidKey for a deactivated team", err)
	}
}

// --- IP allowlist ---

func TestCheckIP(t *testing.T) {
	cases := []struct {
		name      string
		ip        string
		allow     []string
		wantAllow bool
	}{
		{"empty allowlist permits all", "8.8.8.8", nil, true},
		{"exact match", "10.0.0.5", []string{"10.0.0.5"}, true},
		{"inside v4 range", "10.0.0.9", []string{"10.0.0.0/24"}, true},
		{"outside v4 range", "10.0.1.9", []string{"10.0.0.0/24"}, false},
		{"ipv6 exact", "::1", []string{"::1"}, true},
		{"inside v6 range", "2001:db8::5", []string{"2001:db8::/32"}, true},
		{"outside v6 range", "2001:dba::5", []string{"2001:db8::/32"}, false},
		{"second range matches", "192.168.1.1", []string{"10.0.0.0/8", "192.168.0.0/16"}, true},
		{"unparseable address denied", "not-an-ip", []string{"10.0.0.0/8"}, false},
		{"unparseable range ignored", "10.0.0.1", []string{"garbage"}, false},
		{"ipv4 mapped ipv6", "::ffff:10.0.0.5", []string{"10.0.0.0/8"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CheckIP(tc.ip, tc.allow); got != tc.wantAllow {
				t.Errorf("CheckIP(%q, %v) = %v, want %v", tc.ip, tc.allow, got, tc.wantAllow)
			}
		})
	}
}

func TestClientIPIgnoresForwardedHeaderByDefault(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")

	if got := ClientIP(r, false); got != "10.0.0.1" {
		t.Errorf("ClientIP = %q, want the socket address; a forged header must not be trusted", got)
	}
	if got := ClientIP(r, true); got != "1.2.3.4" {
		t.Errorf("ClientIP with trustProxy = %q, want 1.2.3.4", got)
	}
}

func TestClientIPTakesFirstForwardedHop(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8, 9.10.11.12")
	if got := ClientIP(r, true); got != "1.2.3.4" {
		t.Errorf("ClientIP = %q, want the first hop 1.2.3.4", got)
	}
}

// --- HMAC ---

func TestVerifyHMAC(t *testing.T) {
	secret := "s3cret"
	body := []byte(`{"model":"gpt-4"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !VerifyHMAC(secret, body, sig) {
		t.Error("valid signature rejected")
	}
	if VerifyHMAC(secret, body, "sha256=deadbeef") {
		t.Error("invalid signature accepted")
	}
	if VerifyHMAC("wrong-secret", body, sig) {
		t.Error("signature accepted under the wrong secret")
	}
	// A tampered body must invalidate the signature.
	if VerifyHMAC(secret, []byte(`{"model":"evil"}`), sig) {
		t.Error("signature accepted for a modified body")
	}
}

// --- Ledger ---

func TestLedgerAttributesUsage(t *testing.T) {
	admin := &Identity{Role: RoleAdmin}
	if h, p := admin.Ledger("sk-pico-master"); h != "sk-pico-master" || p != "master" {
		t.Errorf("admin ledger = (%q, %q)", h, p)
	}
	user := &Identity{Role: RoleUser, KeyHash: "abc", KeyPrefix: "sk-pico-abc..."}
	if h, p := user.Ledger("sk-pico-master"); h != "abc" || p != "sk-pico-abc..." {
		t.Errorf("user ledger = (%q, %q)", h, p)
	}
}

func ptr(v int64) *int64 { return &v }
