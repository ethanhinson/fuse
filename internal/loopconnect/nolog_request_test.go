package loopconnect

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loggingMethods are logger method names whose arguments get formatted.
var loggingMethods = map[string]bool{
	"Debug": true, "Info": true, "Warn": true, "Error": true, "Log": true,
	"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
	"LogAttrs": true, "With": true, "Print": true, "Printf": true, "Println": true,
	"Fatal": true, "Fatalf": true, "Panic": true, "Panicf": true,
}

// isFormattingCall reports whether call is a fmt/log/slog package call or a
// logger method call that formats its arguments.
func isFormattingCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if id, ok := sel.X.(*ast.Ident); ok && (id.Name == "fmt" || id.Name == "log" || id.Name == "slog") {
		return true
	}
	return loggingMethods[sel.Sel.Name]
}

// wholeRequestRefs returns the bare request values (m, req.Msg) or their
// secrets reached anywhere inside n. A field read such as m.Tenant is fine.
func wholeRequestRefs(n ast.Node) []string {
	var found []string
	parents := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(n, func(x ast.Node) bool {
		if x == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) > 0 {
			parents[x] = stack[len(stack)-1]
		}
		stack = append(stack, x)
		return true
	})
	selectedFrom := func(x ast.Expr) bool {
		p, ok := parents[x].(*ast.SelectorExpr)
		return ok && p.X == x
	}
	ast.Inspect(n, func(x ast.Node) bool {
		switch e := x.(type) {
		case *ast.Ident:
			if e.Name == "m" && !selectedFrom(e) {
				if p, ok := parents[e].(*ast.SelectorExpr); !ok || p.Sel != e {
					found = append(found, "m")
				}
			}
		case *ast.SelectorExpr:
			if id, ok := e.X.(*ast.Ident); ok && id.Name == "req" && e.Sel.Name == "Msg" && !selectedFrom(e) {
				found = append(found, "req.Msg")
			}
			if e.Sel.Name == "Secrets" || e.Sel.Name == "GetSecrets" {
				found = append(found, "secrets")
			}
		}
		return true
	})
	return found
}

// TestNoRequestMessageFormatted guards the wire StartLoopRequest (and its
// LoopSecret values, which the generated String()/prototext/protojson print in
// clear) from ever being formatted or logged by this package.
func TestNoRequestMessageFormatted(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		checked++
		ast.Inspect(f, func(x ast.Node) bool {
			call, ok := x.(*ast.CallExpr)
			if !ok || !isFormattingCall(call) {
				return true
			}
			for _, arg := range call.Args {
				for _, ref := range wholeRequestRefs(arg) {
					t.Errorf("%s: %s is passed to a formatting/logging call; the wire request carries secret values in clear and must never be formatted or logged",
						fset.Position(call.Pos()), ref)
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no non-test Go files found; the guard checked nothing")
	}
}
