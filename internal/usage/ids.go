package usage

import (
	"crypto/rand"
	"encoding/hex"
)

// newRequestID returns a random identifier for a request with no caller-supplied
// id. 16 hex characters matches the Python implementation's length.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}
