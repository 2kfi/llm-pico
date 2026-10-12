// Package org manages teams, users and the model chains that route between
// them, along with the monthly budgets attached to each.
package org

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/arkan/llm-pico/internal/store"
)

// ErrNotFound reports a missing team or user.
var ErrNotFound = errors.New("org: not found")

// Limits are the six per-entity rate-limit ceilings. A nil field is unlimited.
type Limits struct {
	RPM *int64 `json:"rpm_limit,omitempty"`
	RPD *int64 `json:"rpd_limit,omitempty"`
	TPM *int64 `json:"tpm_limit,omitempty"`
	TPD *int64 `json:"tpd_limit,omitempty"`
	ASH *int64 `json:"ash_limit,omitempty"`
	ASD *int64 `json:"asd_limit,omitempty"`
}

// Merge returns the most restrictive combination of a and b at each window.
// nil means unlimited and never constrains.
func (l Limits) Merge(other Limits) Limits {
	return Limits{
		RPM: minPtr(l.RPM, other.RPM),
		RPD: minPtr(l.RPD, other.RPD),
		TPM: minPtr(l.TPM, other.TPM),
		TPD: minPtr(l.TPD, other.TPD),
		ASH: minPtr(l.ASH, other.ASH),
		ASD: minPtr(l.ASD, other.ASD),
	}
}

func minPtr(a, b *int64) *int64 {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *b < *a:
		return b
	default:
		return a
	}
}

// Team is a billing and policy boundary owning a set of users.
type Team struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	Description   *string  `json:"description"`
	IsActive      bool     `json:"is_active"`
	CreatedAt     string   `json:"created_at"`
	Allowlist     []string `json:"model_allowlist"`
	Limits        `json:"-"`
	LimitsOut     Limits   `json:"limits"`
	MonthlyBudget *float64 `json:"monthly_budget_usd"`
	ModelChain    []string `json:"model_chain,omitempty"`
	ChainRewrites *string  `json:"chain_rewrites_response"`
}

// User belongs to exactly one team and may carry its own tighter limits.
type User struct {
	ID            int64    `json:"id"`
	TeamID        int64    `json:"team_id"`
	Email         string   `json:"email"`
	Name          string   `json:"name"`
	IsActive      bool     `json:"is_active"`
	CreatedAt     string   `json:"created_at"`
	Allowlist     []string `json:"model_allowlist"`
	Limits        `json:"-"`
	LimitsOut     Limits   `json:"limits"`
	MonthlyBudget *float64 `json:"monthly_budget_usd"`
}

// --- teams ---

// CreateTeam inserts a team.
func CreateTeam(ctx context.Context, db *store.Store, name string, description *string) (*Team, error) {
	res, err := db.Exec(ctx,
		`INSERT INTO teams (name, description, is_active, created_at) VALUES (?, ?, 1, ?)`,
		name, description, store.Now())
	if err != nil {
		return nil, fmt.Errorf("org: create team: %w", err)
	}
	id, _ := res.LastInsertId()
	return &Team{ID: id, Name: name, Description: description, IsActive: true, CreatedAt: store.Now()}, nil
}

// ListTeams returns every team, newest first.
func ListTeams(ctx context.Context, db *store.Store) ([]Team, error) {
	rows, err := db.Query(ctx, teamSelect+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	return scanTeams(rows)
}

// LoadTeam fetches one team by id.
func LoadTeam(ctx context.Context, db *store.Store, id int64) (*Team, error) {
	row, err := db.QueryRow(ctx, teamSelect+` WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	t, err := scanTeam(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// UpdateTeamLimits sets each supplied window, leaving omitted ones untouched.
// A nil value clears a limit.
func UpdateTeamLimits(ctx context.Context, db *store.Store, id int64, l Limits) (bool, error) {
	return updateLimits(ctx, db, "teams", id, l)
}

// UpdateUserLimits is UpdateTeamLimits for a user.
func UpdateUserLimits(ctx context.Context, db *store.Store, id int64, l Limits) (bool, error) {
	return updateLimits(ctx, db, "users", id, l)
}

// updateLimits applies COALESCE so unset windows keep their current value.
func updateLimits(ctx context.Context, db *store.Store, table string, id int64, l Limits) (bool, error) {
	// table is a compile-time constant at every call site; it is never derived
	// from request input.
	query := fmt.Sprintf(`UPDATE %s SET
	    rpm_limit=COALESCE(?, rpm_limit), rpd_limit=COALESCE(?, rpd_limit),
	    tpm_limit=COALESCE(?, tpm_limit), tpd_limit=COALESCE(?, tpd_limit),
	    ash_limit=COALESCE(?, ash_limit), asd_limit=COALESCE(?, asd_limit)
	  WHERE id=?`, table)
	res, err := db.Exec(ctx, query, l.RPM, l.RPD, l.TPM, l.TPD, l.ASH, l.ASD, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetTeamAllowlist replaces a team's model allowlist. A nil slice clears it.
func SetTeamAllowlist(ctx context.Context, db *store.Store, id int64, models []string) (bool, error) {
	return setAllowlist(ctx, db, "teams", id, models)
}

// SetUserAllowlist is SetTeamAllowlist for a user.
func SetUserAllowlist(ctx context.Context, db *store.Store, id int64, models []string) (bool, error) {
	return setAllowlist(ctx, db, "users", id, models)
}

func setAllowlist(ctx context.Context, db *store.Store, table string, id int64, models []string) (bool, error) {
	encoded, err := encodeAllowlist(models)
	if err != nil {
		return false, err
	}
	query := fmt.Sprintf(`UPDATE %s SET model_allowlist = ? WHERE id = ?`, table)
	res, err := db.Exec(ctx, query, encoded, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetUserBudget sets a user's monthly spend ceiling. nil removes it.
func SetUserBudget(ctx context.Context, db *store.Store, id int64, budget *float64) (bool, error) {
	res, err := db.Exec(ctx, `UPDATE users SET monthly_budget_usd = ? WHERE id = ?`, budget, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeactivateTeam cascades: deactivating a team disables its users and their
// keys, so revoking access takes effect immediately for every issued key.
func DeactivateTeam(ctx context.Context, db *store.Store, id int64) (bool, error) {
	var ok bool
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE teams SET is_active = 0 WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil {
			ok = n > 0
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET is_active = 0 WHERE team_id = ? AND is_active = 1`, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE user_keys SET is_active = 0
			WHERE user_id IN (SELECT id FROM users WHERE team_id = ?) AND is_active = 1`, id)
		return err
	})
	return ok, err
}

// --- users ---

// CreateUser inserts a user into a team.
func CreateUser(ctx context.Context, db *store.Store, teamID int64, email, name string) (*User, error) {
	now := store.Now()
	res, err := db.Exec(ctx,
		`INSERT INTO users (team_id, email, name, is_active, created_at) VALUES (?, ?, ?, 1, ?)`,
		teamID, email, name, now)
	if err != nil {
		return nil, fmt.Errorf("org: create user: %w", err)
	}
	id, _ := res.LastInsertId()
	return &User{ID: id, TeamID: teamID, Email: email, Name: name, IsActive: true, CreatedAt: now}, nil
}

// ListUsers returns users, optionally restricted to one team.
func ListUsers(ctx context.Context, db *store.Store, teamID *int64) ([]User, error) {
	query := userSelect
	args := []any{}
	if teamID != nil {
		query += ` WHERE team_id = ?`
		args = append(args, *teamID)
	}
	query += ` ORDER BY created_at DESC`

	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		u, err := scanUserRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// LoadUser fetches one user by id.
func LoadUser(ctx context.Context, db *store.Store, id int64) (*User, error) {
	row, err := db.QueryRow(ctx, userSelect+` WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	u, err := scanUserRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// Resolve returns a user together with their owning team. Either may be nil
// if the row was removed between lookups.
func Resolve(ctx context.Context, db *store.Store, userID int64) (*User, *Team, error) {
	user, err := LoadUser(ctx, db, userID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	team, err := LoadTeam(ctx, db, user.TeamID)
	if errors.Is(err, ErrNotFound) {
		return user, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return user, team, nil
}

// AssignKeyToUser moves a key between users; a nil userID detaches it.
func AssignKeyToUser(ctx context.Context, db *store.Store, keyHash string, userID *int64) (bool, error) {
	res, err := db.Exec(ctx, `UPDATE user_keys SET user_id = ? WHERE key_hash = ?`, userID, keyHash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// --- chain of LLMs ---

// GetModelChain returns a team's ordered model chain, or nil when unset.
func GetModelChain(ctx context.Context, db *store.Store, teamID int64) ([]string, error) {
	var raw *string
	if err := db.Get(ctx, &raw, `SELECT model_chain FROM teams WHERE id = ?`, teamID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if raw == nil || *raw == "" {
		return nil, nil
	}
	var chain []string
	if err := json.Unmarshal([]byte(*raw), &chain); err != nil {
		return nil, fmt.Errorf("org: decode model chain for team %d: %w", teamID, err)
	}
	return chain, nil
}

// SetModelChain replaces a team's chain.
func SetModelChain(ctx context.Context, db *store.Store, teamID int64, chain []string) error {
	encoded, err := json.Marshal(chain)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `UPDATE teams SET model_chain = ? WHERE id = ?`, string(encoded), teamID)
	return err
}

// GetChainRewrites returns the response-rewrite template for a team's chain.
func GetChainRewrites(ctx context.Context, db *store.Store, teamID int64) (*string, error) {
	var raw *string
	if err := db.Get(ctx, &raw, `SELECT chain_rewrites_response FROM teams WHERE id = ?`, teamID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return raw, nil
}

// SetChainRewrites stores the response-rewrite template for a team's chain.
func SetChainRewrites(ctx context.Context, db *store.Store, teamID int64, text *string) error {
	_, err := db.Exec(ctx, `UPDATE teams SET chain_rewrites_response = ? WHERE id = ?`, text, teamID)
	return err
}

// --- budgets ---

// MonthStart is the timestamp bounding the current calendar month in UTC.
func MonthStart() string { return store.Now()[:7] + "-01T00:00:00" }

// UserMonthSpend totals a user's spend across all of their keys this month.
func UserMonthSpend(ctx context.Context, db *store.Store, userID int64) (float64, error) {
	var total *float64
	err := db.Get(ctx, &total, `
		SELECT COALESCE(SUM(cost_usd), 0) FROM usage_log
		WHERE key_hash IN (SELECT key_hash FROM user_keys WHERE user_id = ?)
		  AND created_at >= ? AND cost_usd IS NOT NULL`,
		userID, MonthStart())
	if err != nil {
		return 0, err
	}
	if total == nil {
		return 0, nil
	}
	return *total, nil
}

// TeamMonthSpend totals a team's spend across all of its users' keys.
func TeamMonthSpend(ctx context.Context, db *store.Store, teamID int64) (float64, error) {
	var total *float64
	err := db.Get(ctx, &total, `
		SELECT COALESCE(SUM(cost_usd), 0) FROM usage_log
		WHERE key_hash IN (
		  SELECT uk.key_hash FROM user_keys uk
		  JOIN users u ON uk.user_id = u.id
		  WHERE u.team_id = ?)
		  AND created_at >= ? AND cost_usd IS NOT NULL`,
		teamID, MonthStart())
	if err != nil {
		return 0, err
	}
	if total == nil {
		return 0, nil
	}
	return *total, nil
}

// CheckUserBudget returns an error message when adding estimatedCost to a
// user's month-to-date spend would exceed their budget.
func CheckUserBudget(ctx context.Context, db *store.Store, userID int64, estimatedCost float64) (string, error) {
	user, err := LoadUser(ctx, db, userID)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if user == nil || user.MonthlyBudget == nil {
		return "", nil
	}
	spend, err := UserMonthSpend(ctx, db, userID)
	if err != nil {
		return "", err
	}
	if spend+estimatedCost > *user.MonthlyBudget {
		return fmt.Sprintf("Monthly budget exceeded: $%.4f + $%.4f > $%.2f",
			spend, estimatedCost, *user.MonthlyBudget), nil
	}
	return "", nil
}

// CheckTeamBudget is CheckUserBudget for a team's aggregate spend.
func CheckTeamBudget(ctx context.Context, db *store.Store, teamID int64, estimatedCost float64) (string, error) {
	team, err := LoadTeam(ctx, db, teamID)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if team == nil || team.MonthlyBudget == nil {
		return "", nil
	}
	spend, err := TeamMonthSpend(ctx, db, teamID)
	if err != nil {
		return "", err
	}
	if spend+estimatedCost > *team.MonthlyBudget {
		return fmt.Sprintf("Team budget exceeded: $%.4f + $%.4f > $%.2f",
			spend, estimatedCost, *team.MonthlyBudget), nil
	}
	return "", nil
}

// --- scanning helpers ---

const teamSelect = `
	SELECT id, name, description, is_active, created_at, model_allowlist,
	       rpm_limit, rpd_limit, tpm_limit, tpd_limit, ash_limit, asd_limit,
	       monthly_budget_usd, model_chain, chain_rewrites_response
	FROM teams`

const userSelect = `
	SELECT id, team_id, email, name, is_active, created_at, model_allowlist,
	       rpm_limit, rpd_limit, tpm_limit, tpd_limit, ash_limit, asd_limit,
	       monthly_budget_usd
	FROM users`

type scanner interface {
	Scan(dest ...any) error
}

func scanTeam(s scanner) (*Team, error) {
	var (
		t         Team
		desc      *string
		allowlist *string
		l         Limits
		chain     *string
		rewrites  *string
	)
	err := s.Scan(&t.ID, &t.Name, &desc, &t.IsActive, &t.CreatedAt, &allowlist,
		&l.RPM, &l.RPD, &l.TPM, &l.TPD, &l.ASH, &l.ASD,
		&t.MonthlyBudget, &chain, &rewrites)
	if err != nil {
		return nil, err
	}
	t.Description = desc
	t.Allowlist = decodeAllowlist(allowlist)
	t.Limits = l
	t.LimitsOut = l
	t.ChainRewrites = rewrites
	if chain != nil && *chain != "" {
		_ = json.Unmarshal([]byte(*chain), &t.ModelChain)
	}
	return &t, nil
}

func scanTeams(rows *sql.Rows) ([]Team, error) {
	defer rows.Close()
	var out []Team
	for rows.Next() {
		t, err := scanTeam(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func scanUserRow(s scanner) (*User, error) {
	var (
		u         User
		allowlist *string
		l         Limits
	)
	err := s.Scan(&u.ID, &u.TeamID, &u.Email, &u.Name, &u.IsActive, &u.CreatedAt, &allowlist,
		&l.RPM, &l.RPD, &l.TPM, &l.TPD, &l.ASH, &l.ASD, &u.MonthlyBudget)
	if err != nil {
		return nil, err
	}
	u.Allowlist = decodeAllowlist(allowlist)
	u.Limits = l
	u.LimitsOut = l
	return &u, nil
}

func scanUserRows(rows *sql.Rows) (*User, error) { return scanUserRow(rows) }

func decodeAllowlist(s *string) []string {
	if s == nil || *s == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(*s), &out); err != nil {
		return nil
	}
	return out
}

func encodeAllowlist(models []string) (*string, error) {
	if models == nil {
		return nil, nil
	}
	b, err := json.Marshal(models)
	if err != nil {
		return nil, err
	}
	s := string(b)
	return &s, nil
}
