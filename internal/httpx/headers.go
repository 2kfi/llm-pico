package httpx

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/arkan/llm-pico/internal/ratelimit"
)

// rateLimitWindows is the header order used when reporting limits, so clients
// see a stable sequence.
var rateLimitWindows = []ratelimit.Window{
	ratelimit.RPM, ratelimit.RPD, ratelimit.TPM,
	ratelimit.TPD, ratelimit.ASH, ratelimit.ASD,
}

// headerName renders a window as its X-RateLimit-* suffix, e.g. RPM.
func headerName(w ratelimit.Window) string {
	return strings.ToUpper(string(w))
}

// RateLimitHeaders renders X-RateLimit-* headers describing current usage.
//
// The same limits that gate a request are reported back to the caller, so a
// client can pace itself without guessing. With no limiter or no configured
// limits, the result is empty rather than an error: an admin key has no
// per-key limits, and reporting none is the honest answer.
func RateLimitHeaders(limiter *ratelimit.Limiter, keyHash, modelName string, level ratelimit.Level, limits ratelimit.Limits) http.Header {
	h := http.Header{}
	if limiter == nil || !limits.HasAny() {
		return h
	}
	now := time.Now()
	for _, w := range rateLimitWindows {
		limit, ok := limits[w]
		if !ok || limit == nil {
			continue
		}
		used := limiter.Usage(keyHash, modelName, level, w)
		remaining := *limit - used
		if remaining < 0 {
			remaining = 0
		}
		name := headerName(w)
		h.Set("X-RateLimit-"+name+"-Limit", strconv.FormatInt(*limit, 10))
		h.Set("X-RateLimit-"+name+"-Remaining", strconv.FormatInt(remaining, 10))
		h.Set("X-RateLimit-"+name+"-Reset", strconv.FormatInt(ratelimit.WindowEnd(w, now).Unix(), 10))
	}
	return h
}

// Apply copies the given headers onto a response writer.
func Apply(w http.ResponseWriter, headers http.Header) {
	for k, vs := range headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}

// ModelListResponse renders the OpenAI-shaped model listing.
func ModelListResponse(names []string) map[string]any {
	created := time.Now().Unix()
	data := make([]map[string]any, 0, len(names))
	for _, n := range names {
		data = append(data, map[string]any{
			"id":       n,
			"object":   "model",
			"created":  created,
			"owned_by": "llm-pico",
		})
	}
	return map[string]any{"object": "list", "data": data}
}

// ChainHeaders describes which models a Chain-of-LLMs hop actually tried.
//
// They are returned even on success, because a caller pinned to an expensive
// model needs to know it silently landed on a cheaper one.
func ChainHeaders(actualModel string, tried []string, hops int) http.Header {
	h := http.Header{}
	if actualModel != "" {
		h.Set("X-Actual-Model", actualModel)
	}
	if hops > 0 {
		h.Set("X-Chain-Hops", strconv.Itoa(hops))
	}
	if len(tried) > 0 {
		h.Set("X-Chain-Tried", strings.Join(tried, ","))
	}
	return h
}

// LogRequest renders one line for the structured access log.
func LogRequest(method, path string, status int, latency time.Duration) string {
	return fmt.Sprintf("%s %s %d %s", method, path, status, latency.Round(time.Millisecond))
}

// SortedKeys returns map keys in a stable order, so generated output such as
// the Prometheus endpoint is reproducible.
func SortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ApplyHeader copies src onto dst, replacing any existing values for the same
// key. This is how per-attempt headers such as rate-limit state and chain
// metadata are layered onto a response.
func ApplyHeader(dst, src http.Header) {
	for k, vs := range src {
		dst[k] = vs
	}
}
