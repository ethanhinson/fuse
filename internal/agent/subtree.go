package agent

// SessionTokens reports the total tokens (in+out) charged across the WHOLE tree,
// root INCLUDED — the session's lifetime spend (change 0036). The optional
// throughput.session_tokens ceiling is measured against this. Unlike the subtree
// sums (which exclude their holder root), the session total counts every node
// because there is no holder to exclude — the session ceiling caps all spend.
func (t *AgentTree) SessionTokens() int {
	total := 0
	for _, v := range t.SnapshotAll() {
		total += v.TokensIn + v.TokensOut
	}
	return total
}
