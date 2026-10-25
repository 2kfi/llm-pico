package providers

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// secondDuration keeps the numeric durations above readable without importing
// time into every call site that only needs a literal.
const secondDuration = time.Second

// newID returns a random hex identifier for synthesized response ids.
func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is unrecoverable but must not crash the proxy;
		// a zero id keeps the response shape valid.
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}
