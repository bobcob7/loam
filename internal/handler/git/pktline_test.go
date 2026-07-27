package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPktLine_ExactBytes pins the pkt-line encoding of the two service
// header lines this handler ever hand-writes to the EXACT byte sequence a
// real git client requires: a 4-hex-digit length prefix counting the
// prefix itself plus the payload, immediately followed by the payload
// verbatim. This is the "classic failure" this bead's own instructions
// call out -- an off-by-one in the length (e.g. counting only the
// payload, or forgetting the trailing newline) produces a confusing
// client-side error rather than a clean test failure, so the expected
// values here are computed independently (by hand, cross-checked with
// python's len()) rather than by calling pktLine with a different
// expression that could share the same bug.
func TestPktLine_ExactBytes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []byte("001e# service=git-upload-pack\n"), pktLine("# service=git-upload-pack\n"))
	assert.Equal(t, []byte("001f# service=git-receive-pack\n"), pktLine("# service=git-receive-pack\n"))
}

// TestFlushPkt_ExactBytes pins the flush-pkt to exactly "0000" -- four
// ASCII characters, no trailing anything. A mutation appending a newline
// or using the numeric length differently would still "look flushy" to a
// casual read but corrupts the framing a real git client parses byte-for-
// byte.
func TestFlushPkt_ExactBytes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []byte("0000"), flushPkt)
}
