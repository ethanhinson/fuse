package agent

// human_testsupport.go exposes narrow constructors so other packages' tests can
// build a tree with child nodes without going through the full spawn machinery.
// These are test-only conveniences kept in a normal file (not _test.go) so they
// are importable from sibling packages' tests (e.g. internal/tui).

// NewAgentNodeForTest builds a running AgentNode with the given id/parent/label.
func NewAgentNodeForTest(id, parentID, label string) *AgentNode {
	return &AgentNode{ID: id, ParentID: parentID, Label: label, Status: StatusRunning}
}

// AddNodeForTest inserts a pre-built node into the tree (test-only).
func (t *AgentTree) AddNodeForTest(n *AgentNode) { t.addNode(n) }
