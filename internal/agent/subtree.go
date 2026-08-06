package agent

// Workflow subtree accounting (change 0034).
//
// A workflow's pool is enforced over the SUBTREE rooted at the node whose
// WorkflowRoot marker is set. These accessors scope the tree's global counts to
// one such subtree so a workflow strip predicate can read exactly the children
// its pool governs — never charging a sibling non-workflow branch.
//
// All three build a single SnapshotAll() view (race-safe, each node locked
// internally) and walk parent chains within it, so they never hold the tree lock
// across node locks.

// parentIndex builds id->ParentID and id->NodeView maps from a snapshot.
func parentIndex(views []NodeView) (parent map[string]string, byID map[string]NodeView) {
	parent = make(map[string]string, len(views))
	byID = make(map[string]NodeView, len(views))
	for _, v := range views {
		parent[v.ID] = v.ParentID
		byID[v.ID] = v
	}
	return parent, byID
}

// inSubtree reports whether id is rootID or a descendant of rootID, walking the
// parent chain in the given index. A cycle-guard bounds the walk by node count.
func inSubtree(parent map[string]string, rootID, id string) bool {
	if id == rootID {
		return true
	}
	cur := id
	for steps := 0; steps < len(parent)+1; steps++ {
		p, ok := parent[cur]
		if !ok || p == "" {
			return false
		}
		if p == rootID {
			return true
		}
		cur = p
	}
	return false
}

// InSubtree reports whether id is rootID itself or a descendant of it.
func (t *AgentTree) InSubtree(rootID, id string) bool {
	parent, _ := parentIndex(t.SnapshotAll())
	return inSubtree(parent, rootID, id)
}

// SubtreeActiveCounts reports running and pending child counts within the
// subtree rooted at rootID, EXCLUDING the root node itself (the holder) — the
// direct analogue of ActiveCounts, which excludes the tree root (Depth==0).
// This is the reversible-cap input for a workflow's concurrent pool.
func (t *AgentTree) SubtreeActiveCounts(rootID string) (running, pending int) {
	views := t.SnapshotAll()
	parent, _ := parentIndex(views)
	for _, v := range views {
		if v.ID == rootID {
			continue
		}
		if !inSubtree(parent, rootID, v.ID) {
			continue
		}
		switch v.Status {
		case StatusRunning:
			running++
		case StatusPending:
			pending++
		}
	}
	return running, pending
}

// SubtreeSpawnCount reports how many nodes exist within the subtree rooted at
// rootID, EXCLUDING the root itself — i.e. how many children the workflow has
// spawned over its life. This is the permanent-quota input for a workflow's
// total pool (the tree is append-only, so it only grows).
func (t *AgentTree) SubtreeSpawnCount(rootID string) int {
	views := t.SnapshotAll()
	parent, _ := parentIndex(views)
	count := 0
	for _, v := range views {
		if v.ID == rootID {
			continue
		}
		if inSubtree(parent, rootID, v.ID) {
			count++
		}
	}
	return count
}

// WorkflowRootOf returns the id of the nearest ancestor-or-self of id whose
// WorkflowRoot marker is set, or "" when id is not inside any workflow subtree.
// When workflows nest, the INNERMOST (nearest) root wins — the tighter policy.
func (t *AgentTree) WorkflowRootOf(id string) string {
	views := t.SnapshotAll()
	parent, byID := parentIndex(views)
	cur := id
	for steps := 0; steps < len(parent)+1; steps++ {
		v, ok := byID[cur]
		if !ok {
			return ""
		}
		if v.WorkflowRoot != "" {
			return v.ID
		}
		if v.ParentID == "" {
			return ""
		}
		cur = v.ParentID
	}
	return ""
}
