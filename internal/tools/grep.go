package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/workspace"
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

type grepTool struct {
	fsys workspace.FileSystem
}

// NewGrep returns the grep tool: regex content search over a file tree.
// When fsys is nil the tool falls back to raw os.*.
func NewGrep(fsys ...workspace.FileSystem) Tool {
	t := grepTool{}
	if len(fsys) > 0 {
		t.fsys = fsys[0]
	}
	return t
}

func (grepTool) Name() string { return "grep" }

func (grepTool) Description() string {
	return "Search file contents with a Go regex; returns path:line: text matches. " +
		"Use this to LOCATE code, then read_file with start_line/end_line for the parts you need — " +
		"much cheaper than reading whole files. path may also be a full art:// artifact ref (e.g. from a spilled-output marker)."
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
				"description": "File or directory to search (default: current directory, recursive), or a full art:// artifact ref.",
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

func (t grepTool) Execute(ctx context.Context, args string) Result {
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

	if artifact.IsRef(a.Path) {
		return grepArtifact(ctx, a.Path, re, maxResults)
	}

	p := stripWorkspacePrefix(a.Path)

	if t.fsys != nil {
		return t.grepFS(ctx, p, re, a.Glob, maxResults)
	}
	return t.grepOS(ctx, p, re, a.Glob, maxResults)
}

// grepFS searches using the workspace FileSystem.
func (t grepTool) grepFS(ctx context.Context, root string, re *regexp.Regexp, glob string, maxResults int) Result {
	var out []string
	truncated := false

	walk := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // permission-denied-like errors are silently skipped
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(out) >= maxResults {
			truncated = true
			return filepath.SkipAll
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if glob != "" {
			if ok, _ := filepath.Match(glob, d.Name()); !ok {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || info.Size() > grepMaxFileSize {
			return nil
		}
		matches, merr := grepFileFS(t.fsys, path, re, maxResults-len(out))
		if merr != nil {
			return nil // unreadable/binary files are silently skipped
		}
		out = append(out, matches...)
		return nil
	}

	// Check if root is a file or directory.
	info, err := t.fsys.Stat(root)
	if err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("grep: %v", err)}
	}
	if info.IsDir() {
		err = t.fsys.Walk(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if grepSkipDirs[d.Name()] || (strings.HasPrefix(d.Name(), ".") && path != root) {
					return filepath.SkipDir
				}
				return nil
			}
			return walk(path, d, nil)
		})
		if err != nil && err != filepath.SkipAll && ctx.Err() == nil {
			return Result{IsError: true, Output: fmt.Sprintf("grep: %v", err)}
		}
	} else {
		matches, merr := grepFileFS(t.fsys, root, re, maxResults)
		if merr != nil {
			return Result{IsError: true, Output: fmt.Sprintf("grep: %v", merr)}
		}
		out = matches
	}
	return grepResult(out, truncated, maxResults)
}

// grepOS searches using raw os.* (fallback when no workspace is wired).
func (t grepTool) grepOS(ctx context.Context, root string, re *regexp.Regexp, glob string, maxResults int) Result {
	var out []string
	truncated := false

	walk := func(path string, d fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(out) >= maxResults {
			truncated = true
			return filepath.SkipAll
		}
		if glob != "" {
			if ok, _ := filepath.Match(glob, d.Name()); !ok {
				return nil
			}
		}
		if info, err := d.Info(); err != nil || info.Size() > grepMaxFileSize {
			return nil
		}
		matches, err := grepFileOS(path, re, maxResults-len(out))
		if err != nil {
			return nil // unreadable/binary files are silently skipped
		}
		out = append(out, matches...)
		return nil
	}

	info, err := os.Stat(root)
	if err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("grep: %v", err)}
	}
	if info.IsDir() {
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if grepSkipDirs[d.Name()] || (strings.HasPrefix(d.Name(), ".") && path != root) {
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
		matches, merr := grepFileOS(root, re, maxResults)
		if merr != nil {
			return Result{IsError: true, Output: fmt.Sprintf("grep: %v", merr)}
		}
		out = matches
	}
	return grepResult(out, truncated, maxResults)
}

// grepResult renders matches with the shared cap footer.
func grepResult(out []string, truncated bool, maxResults int) Result {
	if len(out) == 0 {
		return Result{Output: "no matches"}
	}
	res := strings.Join(out, "\n")
	if truncated || len(out) >= maxResults {
		res += fmt.Sprintf("\n[grep: showing first %d matches — narrow the pattern, path, or glob for more precision]", len(out))
	}
	return Result{Output: res}
}

// grepArtifact scans one artifact's lines, resolved through the loop's binding.
// The ref string stands in for the path in every "path:line: text" entry.
func grepArtifact(ctx context.Context, ref string, re *regexp.Regexp, maxResults int) Result {
	if _, err := artifact.ParseRef(ref); err != nil {
		if _, perr := artifact.ParsePrefix(ref); perr == nil {
			return Result{IsError: true, Output: fmt.Sprintf(
				"grep: %s is not a full artifact ref; pass art://<tenant>/<loop>/<kind>/<id> (list_directory shows them)", ref)}
		}
	}
	data, _, err := readArtifact(ctx, ref)
	if err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("grep: %v", err)}
	}
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return Result{IsError: true, Output: fmt.Sprintf("grep: %s: binary file", ref)}
	}
	var out []string
	for i, line := range strings.Split(string(data), "\n") {
		if !re.MatchString(line) {
			continue
		}
		out = append(out, grepEntry(ref, i+1, line))
		if len(out) >= maxResults {
			break
		}
	}
	return grepResult(out, false, maxResults)
}

// grepEntry formats one "path:line: text" match, trimming long lines.
func grepEntry(path string, lineNo int, line string) string {
	text := strings.TrimSpace(line)
	if len(text) > grepMaxLineLen {
		text = truncateBytes(text, grepMaxLineLen) + "…"
	}
	return fmt.Sprintf("%s:%d: %s", path, lineNo, text)
}

// grepFileFS scans one file through the workspace FileSystem for re.
func grepFileFS(fsys workspace.FileSystem, path string, re *regexp.Regexp, limit int) ([]string, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Check for binary content in first 8KB.
	head := make([]byte, 8192)
	n, _ := f.Read(head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return nil, fmt.Errorf("binary file")
	}

	var out []string
	scanner := bufio.NewScanner(io.MultiReader(bytes.NewReader(head[:n]), f))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if !re.MatchString(line) {
			continue
		}
		out = append(out, grepEntry(path, lineNo, line))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// grepFileOS scans one file on the host filesystem for re (nil-fsys fallback).
func grepFileOS(path string, re *regexp.Regexp, limit int) ([]string, error) {
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
		out = append(out, grepEntry(path, lineNo, line))
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
