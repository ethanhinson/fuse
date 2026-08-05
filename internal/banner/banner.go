// Package banner renders the canonical fuse startup banner. It owns one
// plain-ASCII banner string (wordmark, tagline, quickstart commands) and
// exposes it two ways: String for callers that inject it into a buffer (the
// interactive TUI scrollback) and Print for callers writing to an io.Writer.
// No ANSI, no dynamic data — the only variable is the interpolated version.
package banner

import (
	"fmt"
	"io"
)

// tmpl is the canonical banner with a single %s placeholder for the version.
// Every line is spaces, slashes, and letters only — no tabs, no ESC — so it
// renders cleanly through the fixed-width TUI's sanitizeDisplay.
const tmpl = `    ______    __  __   _____   ______
   / ____/   / / / /  / ___/  / ____/
  / /_      / / / /   \__ \  / __/
 / __/     / /_/ /   ___/ / / /___
/_/        \____/   /____/ /_____/

  multi-model agent harness  •  v%s

  fuse run <agent>   start an agent session
  fuse mcps          list connected MCP servers
  fuse help          show all commands
`

// String returns the startup banner with version interpolated. The result
// ends with a single trailing newline.
func String(version string) string {
	return fmt.Sprintf(tmpl, version)
}

// Print writes the startup banner to w.
func Print(w io.Writer, version string) {
	fmt.Fprint(w, String(version))
}
