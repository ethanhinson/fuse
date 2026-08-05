package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// grep result bounds. The point of this tool is cheap LOCATION of code — a
// bounded path:line index the model follows up with ranged read_file calls —
// so output must stay small no matter how big the tree is.
const (
	grepDefaultMaxResults = 50
	grepMaxResults        = 200
	grepMaxLineLen        = 250
	grepMaxFileSize       = 1 << 20 // skip files over 1 MiB (generated/vendored blobs)
)

// grepSkipDirs are never descended into.
var grepSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	".idea": true, ".vscode": true, "dist": true, "build": true,
}

type grepTool struct{}

// NewGrep returns the grep tool: regex content search over a file tree.
func NewGrep() Tool { return grepTool{} }

func (grepTool) Name() string { return "grep" }

func (grepTool) Description() string {
	return "Search file contents with a Go regex; returns path:line: text matches. " +
		"Use this to LOCATE code, then read_file with start_line/end_line for the parts you need — " +
		"much cheaper than reading whole files."
}

func (grepTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "Go/RE2 regular expression to search for (required).",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "File or directory to search (default: current directory, recursive).",
			},
			"glob": map[string]any{
				"type":        "string",
				"description": "Optional filename glob filter, e.g. *.go",
			},
			"case_insensitive": map[string]any{
				"type":        "boolean",
				"description": "Case-insensitive matching (default false).",
			},
			"max_results": map[string]any{
				"type":        "integer",
				"description": "Cap on matches returned (default 50, max 200).",
			},
		},
		"required": []string{"pattern"},
	}
}

type grepArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Glob            string `json:"glob"`
	CaseInsensitive bool   `json:"case_insensitive"`
	MaxResults      int    `json:"max_results"`
}

func (grepTool) Execute(ctx context.Context, args string) Result {
	var a grepArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}
	if a.Pattern == "" {
		return Result{IsError: true, Output: "grep: pattern is required"}
	}
	pat := a.Pattern
	if a.CaseInsensitive {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("grep: bad pattern: %v", err)}
	}
	if a.Path == "" {
		a.Path = "."
	}
	maxResults := a.MaxResults
	if maxResults <= 0 {
		maxResults = grepDefaultMaxResults
	}
	if maxResults > grepMaxResults {
		maxResults = grepMaxResults
	}

	var out []string
	truncated := false
	scanned := 0

	walk := func(path string, d fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(out) >= maxResults {
			truncated = true
			return filepath.SkipAll
		}
		if a.Glob != "" {
			if ok, _ := filepath.Match(a.Glob, d.Name()); !ok {
				return nil
			}
		}
		if info, err := d.Info(); err != nil || info.Size() > grepMaxFileSize {
			return nil
		}
		matches, err := grepFile(path, re, maxResults-len(out))
		if err != nil {
			return nil // unreadable/binary files are silently skipped
		}
		scanned++
		out = append(out, matches...)
		return nil
	}

	info, err := os.Stat(a.Path)
	if err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("grep: %v", err)}
	}
	if info.IsDir() {
		err = filepath.WalkDir(a.Path, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if grepSkipDirs[d.Name()] || (strings.HasPrefix(d.Name(), ".") && path != a.Path) {
					return filepath.SkipDir
				}
				return nil
			}
			return walk(path, d)
		})
		if err != nil && err != filepath.SkipAll && ctx.Err() == nil {
			return Result{IsError: true, Output: fmt.Sprintf("grep: %v", err)}
		}
	} else {
		matches, merr := grepFile(a.Path, re, maxResults)
		if merr != nil {
			return Result{IsError: true, Output: fmt.Sprintf("grep: %v", merr)}
		}
		out = matches
	}

	if len(out) == 0 {
		return Result{Output: "no matches"}
	}
	res := strings.Join(out, "\n")
	if truncated || len(out) >= maxResults {
		res += fmt.Sprintf("\n[grep: showing first %d matches — narrow the pattern, path, or glob for more precision]", len(out))
	}
	return Result{Output: res}
}

// grepFile scans one file for re, returning up to limit "path:line: text"
// entries. Binary files (NUL in the first 8KB) are skipped with an error.
func grepFile(path string, re *regexp.Regexp, limit int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	head := make([]byte, 8192)
	n, _ := f.Read(head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return nil, fmt.Errorf("binary file")
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}

	var out []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if !re.MatchString(line) {
			continue
		}
		text := strings.TrimSpace(line)
		if len(text) > grepMaxLineLen {
			text = truncateBytes(text, grepMaxLineLen) + "…"
		}
		out = append(out, fmt.Sprintf("%s:%d: %s", path, lineNo, text))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// truncateBytes cuts s at n bytes, backing up to a rune boundary.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

var _ Tool = grepTool{}
