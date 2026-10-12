// Package usage records token consumption, computes cost, and answers the
// aggregate queries behind the admin statistics endpoints.
package usage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/arkan/llm-pico/internal/store"
)

// Record is one completed request.
type Record struct {
	KeyHash          string
	KeyPrefix        string
	ModelName        string
	Provider         string
	RequestID        string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	LatencyMS        int
	StatusCode       int
	Error            string
	CostUSD          *float64
	ErrorType        string
}

// Cost computes the dollar cost of a request.
//
// Either rate may be unset, in which case only the other contributes; if both
// are unset the model has no configured pricing and cost is unknown.
func Cost(promptTokens, completionTokens int, costPer1MIn, costPer1MOut *float64) *float64 {
	if costPer1MIn == nil && costPer1MOut == nil {
		return nil
	}
	var total float64
	if costPer1MIn != nil {
		total += float64(promptTokens) / 1_000_000 * *costPer1MIn
	}
	if costPer1MOut != nil {
		total += float64(completionTokens) / 1_000_000 * *costPer1MOut
	}
	return &total
}

// Log writes one usage row and returns the generated request id.
func Log(ctx context.Context, db *store.Store, r Record) (string, error) {
	requestID := r.RequestID
	if requestID == "" {
		requestID = newRequestID()
	}
	var errMsg *string
	if r.Error != "" {
		v := truncate(r.Error, 500)
		errMsg = &v
	}
	var errType *string
	if r.ErrorType != "" {
		errType = &r.ErrorType
	}

	_, err := db.Exec(ctx, `
		INSERT INTO usage_log
		  (key_hash, key_prefix, model_name, provider, request_id,
		   prompt_tokens, completion_tokens, total_tokens,
		   latency_ms, status_code, error, cost_usd, error_type, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.KeyHash, r.KeyPrefix, r.ModelName, r.Provider, requestID,
		r.PromptTokens, r.CompletionTokens, r.TotalTokens,
		r.LatencyMS, r.StatusCode, errMsg, r.CostUSD, errType, store.Now())
	if err != nil {
		return "", err
	}
	return requestID, nil
}

// Error taxonomy. These strings are stored in usage_log.error_type and
// surfaced by /admin/stats/errors, so they are part of the API contract.
const (
	ErrRateLimited   = "rate_limit"
	ErrInvalidKey    = "invalid_key"
	ErrQuotaExceeded = "quota_exceeded"
	ErrOverloaded    = "model_overloaded"
	ErrTimeout       = "timeout"
	ErrContentFilter = "content_filter"
	ErrUpstream      = "upstream_error"
	ErrUnknown       = "unknown"
)

// Classify maps an upstream status and body onto an error type. This drives
// the retry decision, so the distinction between retryable and permanent
// matters more than the label itself.
func Classify(statusCode int, body string) string {
	switch statusCode {
	case 429:
		return ErrRateLimited
	case 401:
		return ErrInvalidKey
	case 402:
		return ErrQuotaExceeded
	case 503:
		return ErrOverloaded
	case 504:
		return ErrTimeout
	}
	if strings.Contains(strings.ToLower(body), "content_filter") {
		return ErrContentFilter
	}
	if statusCode >= 500 {
		return ErrUpstream
	}
	return ErrUnknown
}

// Retryable reports whether another attempt could plausibly succeed.
// Client errors will not change on retry; 429 and 5xx might.
func Retryable(statusCode int) bool {
	return statusCode == 429 || statusCode >= 500
}

// --- aggregate queries ---

// Summary is the headline usage totals.
type Summary struct {
	TotalRequests         int     `json:"total_requests"`
	TotalPromptTokens     int     `json:"total_prompt_tokens"`
	TotalCompletionTokens int     `json:"total_completion_tokens"`
	TotalTokens           int     `json:"total_tokens"`
	TotalCostUSD          float64 `json:"total_cost_usd"`
}

// KeyModelUsage is per-key, per-model usage.
type KeyModelUsage struct {
	KeyPrefix        string   `json:"key_prefix"`
	ModelName        string   `json:"model_name"`
	Requests         int      `json:"requests"`
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	TotalTokens      int      `json:"total_tokens"`
	CostUSD          *float64 `json:"cost_usd"`
}

// Query filters an aggregate usage report.
type Query struct {
	KeyHash  string
	FromDate string
	ToDate   string
	UserID   *int64
	TeamID   *int64
	Limit    int
}

// Stats returns a summary plus a per-key/model breakdown.
//
// User and team filters require joining through user_keys, so the join is
// only built when one of them is actually set.
func Stats(ctx context.Context, db *store.Store, q Query) (Summary, []KeyModelUsage, error) {
	conditions := []string{}
	args := []any{}

	if q.KeyHash != "" {
		conditions = append(conditions, "ul.key_hash = ?")
		args = append(args, q.KeyHash)
	}
	if q.FromDate != "" {
		conditions = append(conditions, "ul.created_at >= ?")
		args = append(args, q.FromDate)
	}
	if q.ToDate != "" {
		conditions = append(conditions, "ul.created_at <= ?")
		args = append(args, q.ToDate)
	}

	joins := ""
	if q.UserID != nil {
		joins += " JOIN user_keys uk ON ul.key_hash = uk.key_hash"
		conditions = append(conditions, "uk.user_id = ?")
		args = append(args, *q.UserID)
	}
	if q.TeamID != nil {
		if q.UserID == nil {
			joins += " JOIN user_keys uk ON ul.key_hash = uk.key_hash"
		}
		joins += " JOIN users u ON uk.user_id = u.id"
		conditions = append(conditions, "u.team_id = ?")
		args = append(args, *q.TeamID)
	}

	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	var summary Summary
	sumQuery := `SELECT
	    COUNT(*) AS total_requests,
	    COALESCE(SUM(ul.prompt_tokens), 0) AS total_prompt_tokens,
	    COALESCE(SUM(ul.completion_tokens), 0) AS total_completion_tokens,
	    COALESCE(SUM(ul.total_tokens), 0) AS total_tokens,
	    COALESCE(SUM(ul.cost_usd), 0) AS total_cost_usd
	  FROM usage_log ul` + joins + where
	if err := db.Get(ctx, &summary, sumQuery, args...); err != nil {
		return summary, nil, err
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	detailQuery := `SELECT
	    ul.key_prefix, ul.model_name, COUNT(*) AS requests,
	    COALESCE(SUM(ul.prompt_tokens), 0) AS prompt_tokens,
	    COALESCE(SUM(ul.completion_tokens), 0) AS completion_tokens,
	    COALESCE(SUM(ul.total_tokens), 0) AS total_tokens,
	    SUM(ul.cost_usd) AS cost_usd
	  FROM usage_log ul` + joins + where + `
	  GROUP BY ul.key_prefix, ul.model_name
	  ORDER BY total_tokens DESC
	  LIMIT ?`

	rows, err := db.Query(ctx, detailQuery, append(append([]any{}, args...), limit)...)
	if err != nil {
		return summary, nil, err
	}
	defer rows.Close()

	var details []KeyModelUsage
	for rows.Next() {
		var d KeyModelUsage
		if err := rows.Scan(&d.KeyPrefix, &d.ModelName, &d.Requests, &d.PromptTokens,
			&d.CompletionTokens, &d.TotalTokens, &d.CostUSD); err != nil {
			return summary, nil, err
		}
		details = append(details, d)
	}
	return summary, details, rows.Err()
}

// ModelUsage is per-model aggregate usage.
type ModelUsage struct {
	ModelName        string  `json:"model_name"`
	Provider         string  `json:"provider"`
	Requests         int     `json:"requests"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	TotalCostUSD     float64 `json:"total_cost_usd"`
}

// TopModels reports the most-used models over a period.
func TopModels(ctx context.Context, db *store.Store, fromDate, toDate string, limit int) ([]ModelUsage, error) {
	if limit <= 0 {
		limit = 10
	}
	conditions, args := dateConditions("", fromDate, toDate)
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}
	query := `SELECT model_name, provider, COUNT(*) AS requests,
	    COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
	    COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
	    COALESCE(SUM(total_tokens), 0) AS total_tokens,
	    COALESCE(SUM(cost_usd), 0) AS total_cost_usd
	  FROM usage_log` + where + `
	  GROUP BY model_name, provider
	  ORDER BY total_tokens DESC
	  LIMIT ?`

	rows, err := db.Query(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ModelUsage
	for rows.Next() {
		var m ModelUsage
		if err := rows.Scan(&m.ModelName, &m.Provider, &m.Requests, &m.PromptTokens,
			&m.CompletionTokens, &m.TotalTokens, &m.TotalCostUSD); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ErrorCount is one error-taxonomy bucket.
type ErrorCount struct {
	ErrorType string `json:"error_type"`
	Count     int    `json:"count"`
	ModelName string `json:"model_name"`
	Provider  string `json:"provider"`
}

// ErrorStats breaks failures down by type, excluding unclassified rows.
func ErrorStats(ctx context.Context, db *store.Store, provider, model, fromDate, toDate string) ([]ErrorCount, error) {
	conditions := []string{"error_type IS NOT NULL", "error_type != ?"}
	args := []any{ErrUnknown}
	if provider != "" {
		conditions = append(conditions, "provider = ?")
		args = append(args, provider)
	}
	if model != "" {
		conditions = append(conditions, "model_name = ?")
		args = append(args, model)
	}
	extra, extraArgs := dateConditions("", fromDate, toDate)
	conditions = append(conditions, extra...)
	args = append(args, extraArgs...)

	query := `SELECT error_type, COUNT(*) AS count, model_name, provider
	  FROM usage_log WHERE ` + strings.Join(conditions, " AND ") + `
	  GROUP BY error_type, model_name, provider
	  ORDER BY count DESC`

	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ErrorCount
	for rows.Next() {
		var e ErrorCount
		if err := rows.Scan(&e.ErrorType, &e.Count, &e.ModelName, &e.Provider); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CostGroup is one bucket of a cost breakdown.
type CostGroup struct {
	Key          string  `json:"key"`
	Requests     int     `json:"requests"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	TotalTokens  int     `json:"total_tokens"`
}

// CostStats breaks spend down by user, model or day.
func CostStats(ctx context.Context, db *store.Store, groupBy, fromDate, toDate string) ([]CostGroup, error) {
	column := map[string]string{
		"user":  "ul.key_prefix",
		"model": "ul.model_name",
		"day":   "DATE(ul.created_at)",
	}[groupBy]
	if column == "" {
		return nil, nil
	}

	conditions, args := dateConditions("ul.", fromDate, toDate)
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}
	query := `SELECT ` + column + ` AS bucket,
	    COUNT(*) AS requests,
	    COALESCE(SUM(ul.cost_usd), 0) AS total_cost_usd,
	    COALESCE(SUM(ul.total_tokens), 0) AS total_tokens
	  FROM usage_log ul` + where + `
	  GROUP BY bucket
	  ORDER BY ` + column + ` DESC`

	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CostGroup
	for rows.Next() {
		var g CostGroup
		if err := rows.Scan(&g.Key, &g.Requests, &g.TotalCostUSD, &g.TotalTokens); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ProviderComparison is one provider's observed efficiency for a model.
type ProviderComparison struct {
	ModelName      string  `json:"model_name"`
	Provider       string  `json:"provider"`
	CostPerMillion float64 `json:"cost_per_million"`
	AvgLatencyMS   float64 `json:"avg_latency_ms"`
	RequestCount   int     `json:"request_count"`
}

// CompareProviders ranks providers for the same model by observed cost, which
// is what backs the cost-aware routing advice in the dashboard.
func CompareProviders(ctx context.Context, db *store.Store) ([]ProviderComparison, error) {
	query := `SELECT model_name, provider,
	    AVG(cost_usd * 1000000.0 / NULLIF(total_tokens, 0)) AS cost_per_m,
	    AVG(latency_ms) AS avg_latency_ms,
	    COUNT(*) AS request_count
	  FROM usage_log
	  WHERE total_tokens > 0 AND cost_usd IS NOT NULL
	  GROUP BY model_name, provider
	  ORDER BY model_name, cost_per_m`

	rows, err := db.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProviderComparison
	for rows.Next() {
		var c ProviderComparison
		if err := rows.Scan(&c.ModelName, &c.Provider, &c.CostPerMillion, &c.AvgLatencyMS, &c.RequestCount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Projection is a straight-line forecast of month-end spend.
type Projection struct {
	CurrentSpend   float64 `json:"current_spend"`
	DaysElapsed    int     `json:"days_elapsed"`
	DailyRate      float64 `json:"daily_rate"`
	ProjectedTotal float64 `json:"projected_total"`
	DaysRemaining  int     `json:"days_remaining"`
}

// Project estimates month-end spend from the run rate so far. It is a linear
// extrapolation, not a forecast: a single large early request skews it.
func Project(ctx context.Context, db *store.Store) (Projection, error) {
	now := time.Now().UTC()
	monthStart := now.Format("2006-01") + "-01T00:00:00"

	var spend float64
	if err := db.Get(ctx, &spend,
		`SELECT COALESCE(SUM(cost_usd), 0) FROM usage_log
		 WHERE created_at >= ? AND cost_usd IS NOT NULL`, monthStart); err != nil {
		return Projection{}, err
	}

	daysElapsed := now.Day()
	daysInMonth := daysInMonth(now.Year(), int(now.Month()))
	rate := spend / float64(max(1, daysElapsed))
	return Projection{
		CurrentSpend:   spend,
		DaysElapsed:    daysElapsed,
		DailyRate:      rate,
		ProjectedTotal: rate * float64(daysInMonth),
		DaysRemaining:  daysInMonth - daysElapsed,
	}, nil
}

// KeyMonthSpend totals one key's spend this month.
func KeyMonthSpend(ctx context.Context, db *store.Store, keyHash string) (float64, error) {
	var total float64
	err := db.Get(ctx, &total, `
		SELECT COALESCE(SUM(cost_usd), 0) FROM usage_log
		WHERE key_hash = ? AND created_at >= ? AND cost_usd IS NOT NULL`,
		keyHash, monthStart())
	return total, err
}

// CheckKeyBudget returns an error message when a key's projected spend would
// exceed its budget.
func CheckKeyBudget(ctx context.Context, db *store.Store, keyHash string, estimatedCost float64) (string, error) {
	row, err := db.QueryRow(ctx,
		`SELECT monthly_budget_usd FROM user_keys WHERE key_hash = ?`, keyHash)
	if err != nil {
		return "", err
	}
	var budget *float64
	if err := row.Scan(&budget); err != nil {
		// No row means the key is unknown or revoked; nothing to enforce.
		return "", nil
	}
	if budget == nil {
		return "", nil
	}
	spend, err := KeyMonthSpend(ctx, db, keyHash)
	if err != nil {
		return "", err
	}
	if spend+estimatedCost > *budget {
		return formatBudgetError("Key", spend, estimatedCost, *budget), nil
	}
	return "", nil
}

func formatBudgetError(kind string, spend, estimate, budget float64) string {
	return fmt.Sprintf("%s budget exceeded: $%.4f + $%.4f > $%.2f", kind, spend, estimate, budget)
}

// --- helpers ---

func dateConditions(prefix, fromDate, toDate string) ([]string, []any) {
	var conditions []string
	var args []any
	if fromDate != "" {
		conditions = append(conditions, prefix+"created_at >= ?")
		args = append(args, fromDate)
	}
	if toDate != "" {
		conditions = append(conditions, prefix+"created_at <= ?")
		args = append(args, toDate)
	}
	return conditions, args
}

func monthStart() string { return time.Now().UTC().Format("2006-01") + "-01T00:00:00" }

func daysInMonth(year, month int) int {
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
