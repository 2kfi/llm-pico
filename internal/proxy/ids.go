package proxy

import (
	"crypto/rand"
	"encoding/hex"
)

// newRequestID returns a random identifier for one request. It is echoed to
// clients as X-Request-Id and ties a reported failure to its trace spans.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000000000"
	}
	return hex.EncodeToString(b[:])
}
