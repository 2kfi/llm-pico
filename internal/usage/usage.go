// Package usage provides cost computation and error classification shared by
// the retry and dispatch paths.
package usage

import (
	"strings"
)

// Error taxonomy shared by the error-enveloping layer.
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
