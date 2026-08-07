package agent

// WorkflowPool is the agent-package mirror of a workflow's pool policy, kept
// here so internal/agent never imports internal/config (the config layer builds
// this from its PoolConfig). Each dimension is 0 = unset (that brake off).
//
// The scheduler's unified visibility predicate (Scheduler.Visible, change 0036)
// consumes a registered WorkflowPool at subtree scope, preserving the change
// 0034 semantics: Total is permanent (lifetime spawn quota for the subtree),
// Concurrent is reversible (max running+pending children in the subtree), and
// MaxDepth is static (a node at rootDepth+MaxDepth can never spawn). The tighter
// of pool scope and global scope governs, and the call-time backstops in Spawn
// remain the race safety net either way.
type WorkflowPool struct {
	Concurrent int // reversible: max running+pending children in the subtree
	Total      int // permanent: lifetime spawn quota for the subtree
	MaxDepth   int // static: spawn depth below the workflow root
	Tokens     int // permanent: lifetime token quota for the subtree (0 = unset)
}
