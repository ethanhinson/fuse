// Command fuse is a multi-model agent harness CLI.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point. It returns a process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "config error: %v\n", err)
		return 1
	}
	reg := registryFromConfig(cfg)

	// Subcommand dispatch (only the ones that are not the default task run).
	if len(args) > 0 {
		switch args[0] {
		case "models":
			for _, name := range reg.Names() {
				mc, _ := reg.Resolve(name)
				marker := "  "
				if name == reg.Default {
					marker = "* "
				}
				fmt.Fprintf(stdout, "%s%s\t%s\n", marker, name, mc.ID)
			}
			return 0
		case "shell":
			return runShell(args[1:], cfg, reg, stdout, stderr)
		case "mcps":
			return runMCPs(args[1:], cfg, stdout, stderr)
		case "mcp-server":
			return runMCPServer(args[1:], cfg, stdout, stderr)
		}
	}

	fs := flag.NewFlagSet("fuse", flag.ContinueOnError)
	fs.SetOutput(stderr)
	modelAlias := fs.String("model", "", "model alias to run (default: config default)")
	verbose := fs.Bool("verbose", false, "render full tool args and output")
	trace := fs.Bool("trace", false, "dump raw gateway JSON to stderr")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "usage: fuse [--model NAME] [--verbose] [--trace] \"<task>\"")
		return 2
	}
	task := rest[0]

	var traceW io.Writer
	if *trace {
		traceW = stderr
	}
	a, modelID, err := buildAgent(cfg, reg, *modelAlias, stdout, *verbose, "", traceW)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	_ = modelID

	_, err = a.Run(context.Background(), []model.Message{{Role: "user", Content: task}})
	if err != nil {
		fmt.Fprintf(stderr, "run error: %v\n", err)
		return 1
	}
	return 0
}
