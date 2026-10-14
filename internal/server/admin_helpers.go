package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/arkan/llm-pico/internal/store"

	"github.com/arkan/llm-pico/internal/httpx"
	"github.com/arkan/llm-pico/internal/usage"
)

// truncate shortens a string for logging and error messages.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// updateAllowlist writes a model allowlist onto rows selected by a predicate.
// Passing a nil slice clears the allowlist, which means "unrestricted".
func updateAllowlist(ctx context.Context, db *store.Store, table, where string, whereArgs []any, models *[]string) (int64, error) {
	// table and where are compile-time constants at every call site.
	query := fmt.Sprintf("UPDATE %s SET model_allowlist = ? WHERE %s", table, where)
	encoded := encodeAllowlist(models)

	res, err := db.Exec(ctx, query, append([]any{encoded}, whereArgs...)...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// teamUsage and userUsage serve the per-entity usage breakdowns.
func (a *adminAPI) teamUsage(w http.ResponseWriter, r *http.Request, _ string) {
	id := pathInt(r, "id")
	summary, details, err := usage.Stats(r.Context(), a.server.db, usage.Query{
		FromDate: r.URL.Query().Get("from"),
		ToDate:   r.URL.Query().Get("to"),
		TeamID:   &id,
		Limit:    queryInt(r, "limit", 100),
	})
	if err != nil {
		httpx.Internal("could not read team usage").Write(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"summary": summary, "details": details})
}

func (a *adminAPI) userUsage(w http.ResponseWriter, r *http.Request, _ string) {
	id := pathInt(r, "id")
	summary, details, err := usage.Stats(r.Context(), a.server.db, usage.Query{
		FromDate: r.URL.Query().Get("from"),
		ToDate:   r.URL.Query().Get("to"),
		UserID:   &id,
		Limit:    queryInt(r, "limit", 100),
	})
	if err != nil {
		httpx.Internal("could not read user usage").Write(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"summary": summary, "details": details})
}
