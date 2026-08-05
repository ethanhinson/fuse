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
// The wordmark is figlet's Banner3-D face: hash-pixel letters carved out of a
// colon-dot background. Every byte is printable ASCII — no tabs, no ESC — so
// it renders cleanly through the fixed-width TUI's sanitizeDisplay.
const tmpl = `'########:'##::::'##::'######::'########:
 ##.....:: ##:::: ##:'##... ##: ##.....::
 ##::::::: ##:::: ##: ##:::..:: ##:::::::
 ######::: ##:::: ##:. ######:: ######:::
 ##...:::: ##:::: ##::..... ##: ##...::::
 ##::::::: ##:::: ##:'##::: ##: ##:::::::
 ##:::::::. #######::. ######:: ########:
..:::::::::.......::::......:::........::

  multi-model agent harness  --  v%s

  fuse "<task>"   run an agent on a one-shot task
  fuse shell      start an interactive agent shell
  fuse help       show all commands
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
