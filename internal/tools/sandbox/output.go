package sandbox

import "fmt"

// Output kept per command. Everything between the two is dropped and counted, so a
// command that prints forever costs a fixed amount of memory rather than the
// harness. Both ends matter: the head says what ran, the tail holds the error that
// ended it. The total stays well above what the bash tool shows inline, because
// the rest is spilled for the model to page through.
const (
	outputHeadBytes = 8 << 20
	outputTailBytes = 8 << 20
)

// boundedOutput is an io.Writer that keeps the first and last bytes written to it.
// It never fails a write: an error would kill the command with a broken pipe for
// the crime of being noisy.
//
// Not safe for concurrent writes. Handing os/exec the same writer for Stdout and
// Stderr gives both one pipe and one copying goroutine.
type boundedOutput struct {
	headMax, tailMax int
	head             []byte
	// tail is a ring, allocated only once head is full, so an ordinary command
	// never pays for it. next is where the next byte lands.
	tail  []byte
	next  int
	total int64
}

func newBoundedOutput() *boundedOutput {
	return &boundedOutput{headMax: outputHeadBytes, tailMax: outputTailBytes}
}

// Write implements io.Writer.
func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	b.total += int64(n)
	if room := b.headMax - len(b.head); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		b.head = append(b.head, p[:room]...)
		p = p[room:]
	}
	if len(p) >= b.tailMax {
		// Only the end of this write can survive it.
		p = p[len(p)-b.tailMax:]
	}
	for len(p) > 0 {
		if len(b.tail) < b.tailMax {
			room := b.tailMax - len(b.tail)
			if room > len(p) {
				room = len(p)
			}
			b.tail = append(b.tail, p[:room]...)
			p = p[room:]
			continue
		}
		c := copy(b.tail[b.next:], p)
		b.next = (b.next + c) % b.tailMax
		p = p[c:]
	}
	return n, nil
}

// Bytes returns what was kept, with a marker saying how much is missing when
// anything is. A model reading a cut-off log must be able to tell that it is one.
func (b *boundedOutput) Bytes() []byte {
	kept := int64(len(b.head) + len(b.tail))
	out := make([]byte, 0, kept+128)
	out = append(out, b.head...)
	if dropped := b.total - kept; dropped > 0 {
		out = append(out, fmt.Sprintf("\n[... %d bytes of output dropped; kept the first %d and last %d bytes ...]\n",
			dropped, len(b.head), len(b.tail))...)
	}
	out = append(out, b.tail[b.next:]...)
	out = append(out, b.tail[:b.next]...)
	return out
}
