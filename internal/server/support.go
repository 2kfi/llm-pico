package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/arkan/llm-pico/internal/observability"
	"github.com/arkan/llm-pico/internal/proxy"
	"github.com/arkan/llm-pico/internal/usage"
)

// prometheus renders the Prometheus exposition text.
func (a *adminAPI) prometheus(r *http.Request) (string, error) {
	return observability.Prometheus(r.Context(), a.server.db)
}

// observabilityTraces loads the span list for a request id.
func (a *adminAPI) traces(ctx context.Context, requestID string) ([]observability.Span, error) {
	return observability.Traces(ctx, a.server.db, requestID)
}

// proxyDegradation validates and converts a degradation mode name.
func proxyDegradation(mode string) proxy.DegradationMode {
	switch proxy.DegradationMode(mode) {
	case proxy.DegradationNormal:
		return proxy.DegradationNormal
	case proxy.DegradationReject:
		return proxy.DegradationReject
	case proxy.DegradationFallbackOnly:
		return proxy.DegradationFallbackOnly
	default:
		return ""
	}
}

// queryFrom builds a usage query from the request's query parameters.
func queryFrom(r *http.Request) usage.Query {
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		limit = queryInt(r, "limit", 100)
	}
	return usage.Query{
		KeyHash:  q.Get("key_hash"),
		FromDate: q.Get("from"),
		ToDate:   q.Get("to"),
		Limit:    limit,
	}
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
