package main

import (
	"io"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
)

// runShell is implemented in Task 9. This stub keeps cmd/fuse compilable and
// is fully replaced there.
func runShell(args []string, cfg config.Config, reg *model.Registry, stdout, stderr io.Writer) int {
	_ = args
	_ = cfg
	_ = reg
	io.WriteString(stderr, "shell not yet implemented\n")
	return 1
}
