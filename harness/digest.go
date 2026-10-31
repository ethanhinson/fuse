package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"google.golang.org/protobuf/proto"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
)

// Digest is a spec's content digest: "sha256:" followed by the hex SHA-256 of
// its deterministic protobuf encoding (DigestBytes over that encoding). A fuse
// server records it with the spec a loop runs (the loop.harness event, change
// 0085) and returns it from StartLoop, so a client can tell which harness a
// loop ran. protobuf-go does not promise its deterministic encoding is
// identical across library versions, so compare digests a server reported for
// the same recorded spec rather than recomputing them elsewhere. A nil spec
// digests as the empty spec.
func Digest(spec *harnessv1.HarnessSpec) string {
	return DigestBytes(Encode(spec))
}

// Encode is the deterministic protobuf encoding Digest is computed over. A nil
// spec encodes as the empty spec.
func Encode(spec *harnessv1.HarnessSpec) []byte {
	if spec == nil {
		spec = &harnessv1.HarnessSpec{}
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(spec)
	if err != nil {
		// A generated message always marshals; failing here is a programming
		// error, not a problem with the spec.
		panic(fmt.Sprintf("harness: marshal spec: %v", err))
	}
	return b
}

// DigestAlgorithm is the digest algorithm id that prefixes every Digest. It is
// the id GetCapabilities advertises for harness spec digests.
const DigestAlgorithm = "sha256"

// DigestBytes is DigestAlgorithm, ":", and the hex SHA-256 of an encoded spec.
func DigestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return DigestAlgorithm + ":" + hex.EncodeToString(sum[:])
}
