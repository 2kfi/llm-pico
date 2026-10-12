package observability

import (
	"context"
	"strconv"
	"strings"

	"github.com/arkan/llm-pico/internal/store"
)

// metricPrefix namespaces every exported series.
const metricPrefix = "llm_pico_"

// Prometheus renders the usage_log table in Prometheus text exposition format
// for scraping at /admin/stats/metrics.
//
// Counters are derived from the usage table rather than kept in memory, so
// metrics survive a restart and stay consistent with the dashboard.
func Prometheus(ctx context.Context, db *store.Store) (string, error) {
	var b strings.Builder

	type requestRow struct {
		Model      string
		Provider   string
		StatusCode int
		Count      int
	}
	rows, err := db.Query(ctx, `
		SELECT model_name, provider, status_code, COUNT(*) AS count
		FROM usage_log GROUP BY model_name, provider, status_code`)
	if err != nil {
		return "", err
	}
	var requests []requestRow
	for rows.Next() {
		var r requestRow
		if err := rows.Scan(&r.Model, &r.Provider, &r.StatusCode, &r.Count); err != nil {
			rows.Close()
			return "", err
		}
		requests = append(requests, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}

	b.WriteString("# HELP " + metricPrefix + "requests_total Total requests by model, provider and status\n")
	b.WriteString("# TYPE " + metricPrefix + "requests_total counter\n")
	for _, r := range requests {
		b.WriteString(metricPrefix + "requests_total{" +
			"model=" + escapeLabel(r.Model) + "," +
			"provider=" + escapeLabel(r.Provider) + "," +
			"status=" + escapeLabel(strconv.Itoa(r.StatusCode)) + "} " + strconv.Itoa(r.Count) + "\n")
	}

	type tokenRow struct {
		Model    string
		Provider string
		Tokens   int
		Cost     float64
	}
	rows, err = db.Query(ctx, `
		SELECT model_name, provider,
		       COALESCE(SUM(total_tokens), 0) AS tokens,
		       COALESCE(SUM(cost_usd), 0) AS cost
		FROM usage_log GROUP BY model_name, provider`)
	if err != nil {
		return "", err
	}
	var tokens []tokenRow
	for rows.Next() {
		var t tokenRow
		if err := rows.Scan(&t.Model, &t.Provider, &t.Tokens, &t.Cost); err != nil {
			rows.Close()
			return "", err
		}
		tokens = append(tokens, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}

	b.WriteString("# HELP " + metricPrefix + "tokens_total Total tokens by model and provider\n")
	b.WriteString("# TYPE " + metricPrefix + "tokens_total counter\n")
	for _, t := range tokens {
		b.WriteString(metricPrefix + "tokens_total{" +
			"model=" + escapeLabel(t.Model) + "," +
			"provider=" + escapeLabel(t.Provider) + "} " + strconv.Itoa(t.Tokens) + "\n")
	}

	b.WriteString("# HELP " + metricPrefix + "cost_usd_total Total spend by model and provider\n")
	b.WriteString("# TYPE " + metricPrefix + "cost_usd_total counter\n")
	for _, t := range tokens {
		b.WriteString(metricPrefix + "cost_usd_total{" +
			"model=" + escapeLabel(t.Model) + "," +
			"provider=" + escapeLabel(t.Provider) + "} " + formatFloat(t.Cost) + "\n")
	}

	type errorRow struct {
		ErrorType string
		Count     int
	}
	rows, err = db.Query(ctx, `
		SELECT error_type, COUNT(*) AS count FROM usage_log
		WHERE error_type IS NOT NULL AND error_type != ''
		GROUP BY error_type`)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	b.WriteString("# HELP " + metricPrefix + "errors_total Total errors by taxonomy class\n")
	b.WriteString("# TYPE " + metricPrefix + "errors_total counter\n")
	for rows.Next() {
		var e errorRow
		if err := rows.Scan(&e.ErrorType, &e.Count); err != nil {
			return "", err
		}
		b.WriteString(metricPrefix + "errors_total{type=" + escapeLabel(e.ErrorType) + "} " + strconv.Itoa(e.Count) + "\n")
	}
	return b.String(), rows.Err()
}

// escapeLabel quotes a Prometheus label value, escaping the characters that
// would otherwise break the exposition format.
func escapeLabel(v string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// formatFloat renders a metric value without an exponent or trailing zeros,
// which Prometheus parses more reliably.
func formatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if s == "-0" {
		return "0"
	}
	return s
}
