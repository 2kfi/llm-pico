package server

import (
	"bytes"
	"io"
)

// newBodyReader returns a fresh reader over an already-consumed body.
//
// HMAC verification has to read the body before the handler can, so the
// handler receives a replacement reader over the same bytes rather than an
// already-drained body.
func newBodyReader(b []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(b))
}
