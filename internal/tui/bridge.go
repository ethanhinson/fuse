package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
)

// StartBridges pumps every session event source into the bubbletea program
// via Program.Send. This replaces the old "waitForMsg re-arm" pattern, where
// Update had to re-subscribe a receiver command for exactly the message types
// each channel produced — an invariant the type system couldn't enforce, and
// whose violations (double-arm, dropped arm) caused permanent freezes and
// message reordering. Program.Send needs no arming: it is safe from any
// goroutine, and returns immediately once the program has quit.
//
// ctx must outlive p.Run(); cancel it after Run returns to stop the pumps.
// tree and slashReg may be nil.
func StartBridges(ctx context.Context, p *tea.Program, ch <-chan tea.Msg, tree *agent.AgentTree, slashReg *SlashRegistry) {
	// Agent events (renderer output, approvals, done/err).
	go func() {
		for {
			select {
			case msg := <-ch:
				p.Send(msg)
			case <-ctx.Done():
				return
			}
		}
	}()

	// Tree updates, with burst coalescing: a chatty child can queue hundreds
	// of updates; collapsing same-node bursts keeps the Update loop from
	// re-rendering once per event.
	if tree != nil {
		go func() {
			for {
				select {
				case u := <-tree.Updates():
					ids := map[string]struct{}{u.NodeID: {}}
					for drained := false; !drained; {
						select {
						case u2 := <-tree.Updates():
							ids[u2.NodeID] = struct{}{}
						default:
							drained = true
						}
					}
					for id := range ids {
						p.Send(treeUpdateMsg{nodeID: id})
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// Slash-registry reloads (skill/MCP provider changes).
	if slashReg != nil {
		go func() {
			for {
				select {
				case <-slashReg.Changes():
					p.Send(registryReloadMsg{})
				case <-ctx.Done():
					return
				}
			}
		}()
	}
}
