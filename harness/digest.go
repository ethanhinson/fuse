package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"google.golang.org/protobuf/proto"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
)

// Digest is a spec's content digest: "sha256:" followed by the hex SHA-256 of
// its deterministic protobuf encoding. A fuse server records it with the spec a
// loop runs (the loop.harness event, change 0085), so a client can tell which
// harness a loop ran by comparing digests. A nil spec digests as the empty spec.
func Digest(spec *harnessv1.HarnessSpec) string {
	if spec == nil {
		spec = &harnessv1.HarnessSpec{}
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(spec)
	if err != nil {
		// A generated message always marshals; failing here is a programming
		// error, not a problem with the spec.
		panic(fmt.Sprintf("harness: marshal spec: %v", err))
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
