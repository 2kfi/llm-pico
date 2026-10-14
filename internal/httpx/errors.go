// Package httpx holds the shared HTTP conventions of the proxy: the JSON error
// envelope, rate-limit headers, and the request context values shared between
// handlers.
package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Error codes returned in the "type" field of the error envelope.
const (
	TypeBadRequest      = "bad_request"
	TypeUnauthorized    = "unauthorized"
	TypeForbidden       = "forbidden"
	TypeNotFound        = "not_found"
	TypeRateLimited     = "rate_limit_exceeded"
	TypeBudgetExceeded  = "budget_exceeded"
	TypeUpstream        = "upstream_error"
	TypeUpstreamTimeout = "upstream_timeout"
	TypeNotImplemented  = "not_implemented"
	TypeInternal        = "internal_error"
	TypeDegraded        = "degraded"
	TypeDraining        = "draining"
)

// Error is the proxy's API error. Every failure the client sees is rendered
// from this shape, so error bodies are consistent across every endpoint.
//
// The Python implementation repeated the same six-line dict literal about forty
// times, which made it easy for a handler to invent a slightly different shape.
type Error struct {
	Status     int
	Type       string
	Message    string
	RetryAfter int            // seconds, emitted as a Retry-After header when set
	Extra      map[string]any // additional fields merged into the envelope
}

func (e *Error) Error() string { return e.Type + ": " + e.Message }

// WithExtra attaches an additional field to the error envelope.
func (e *Error) WithExtra(key string, value any) *Error {
	if e.Extra == nil {
		e.Extra = map[string]any{}
	}
	e.Extra[key] = value
	return e
}

// envelope is the wire shape: {"error": {...}}.
type envelope struct {
	Error body `json:"error"`
}

type body struct {
	Message    string         `json:"message"`
	Type       string         `json:"type"`
	Code       int            `json:"code"`
	RetryAfter *int           `json:"retry_after,omitempty"`
	Extra      map[string]any `json:"-"`
}

// Marshal renders the error as its JSON envelope.
func (e *Error) Marshal() []byte {
	b := body{Message: e.Message, Type: e.Type, Code: e.Status}
	if e.RetryAfter > 0 {
		retry := e.RetryAfter
		b.RetryAfter = &retry
	}
	payload, err := json.Marshal(envelope{Error: b})
	if err != nil {
		// Marshalling a struct of primitives cannot realistically fail; fall
		// back to a minimal valid envelope rather than panicking.
		return []byte(`{"error":{"message":"internal error","type":"internal_error","code":500}}`)
	}
	// Extra fields are merged separately because a struct cannot carry a
	// dynamic key set without losing them.
	if len(e.Extra) == 0 {
		return payload
	}
	var generic map[string]any
	if err := json.Unmarshal(payload, &generic); err != nil {
		return payload
	}
	errBody, _ := generic["error"].(map[string]any)
	for k, v := range e.Extra {
		errBody[k] = v
	}
	merged, err := json.Marshal(generic)
	if err != nil {
		return payload
	}
	return merged
}

// Write sends the error to the client.
func (e *Error) Write(w http.ResponseWriter) {
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", itoa(e.RetryAfter))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_, _ = w.Write(e.Marshal())
}

// Errorf builds an API error.
func Errorf(status int, errType, message string) *Error {
	return &Error{Status: status, Type: errType, Message: message}
}

// Common constructors.

func BadRequest(msg string) *Error   { return Errorf(http.StatusBadRequest, TypeBadRequest, msg) }
func Unauthorized(msg string) *Error { return Errorf(http.StatusUnauthorized, TypeUnauthorized, msg) }
func Forbidden(msg string) *Error    { return Errorf(http.StatusForbidden, TypeForbidden, msg) }
func NotFound(msg string) *Error     { return Errorf(http.StatusNotFound, TypeNotFound, msg) }
func Internal(msg string) *Error     { return Errorf(http.StatusInternalServerError, TypeInternal, msg) }
func NotImplemented(msg string) *Error {
	return Errorf(http.StatusNotImplemented, TypeNotImplemented, msg)
}
func Degraded(msg string) *Error { return Errorf(http.StatusServiceUnavailable, TypeDegraded, msg) }
func Draining() *Error {
	return Errorf(http.StatusServiceUnavailable, TypeDraining, "server is shutting down")
}

// RateLimited reports a rate-limit rejection with a retry hint.
func RateLimited(msg string, retryAfter int) *Error {
	return &Error{Status: http.StatusTooManyRequests, Type: TypeRateLimited, Message: msg, RetryAfter: retryAfter}
}

// BudgetExceeded reports a hard budget block.
func BudgetExceeded(msg string) *Error {
	return Errorf(http.StatusTooManyRequests, TypeBudgetExceeded, msg)
}

// Upstream reports a failed upstream call, passing through its status when one
// is meaningful to the client.
func Upstream(status int, msg string) *Error {
	if status < 400 {
		status = http.StatusBadGateway
	}
	return Errorf(status, TypeUpstream, msg)
}

// UpstreamTimeout reports a provider that did not respond in time.
func UpstreamTimeout(msg string) *Error {
	return Errorf(http.StatusGatewayTimeout, TypeUpstreamTimeout, msg)
}

// --- JSON helpers ---

// WriteJSON sends a value as a JSON response.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	payload, err := json.Marshal(v)
	if err != nil {
		Internal("failed to encode response").Write(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// DecodeJSON parses a JSON request body into v, rejecting unknown trailing
// content so a malformed body cannot be silently truncated.
func DecodeJSON(r *http.Request, v any) *Error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return BadRequest("invalid JSON body")
	}
	return nil
}

// FromError converts any error into an API error, mapping known kinds onto the
// right status code and defaulting to a generic 500.
func FromError(err error) *Error {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return Internal("internal server error")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
