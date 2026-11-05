package tools

import "strings"

// stripWorkspacePrefix removes a leading "/workspace/" so the model can use
// what bash prints. art:// refs and paths without the prefix are never touched.
func stripWorkspacePrefix(p string) string {
	if strings.HasPrefix(p, "/workspace/") {
		return strings.TrimPrefix(p, "/workspace/")
	}
	if p == "/workspace" {
		return "."
	}
	return p
}
