package data

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// telemetryWriteGuard lists the generated insert params structs that carry a
// TelemetryContext field. Every call site must set it: an operation row written
// without telemetry makes its first worker claim report context_missing.
var telemetryWriteGuard = []string{
	"InsertOperationParams",
	"InsertPlatformOperationParams",
	"InsertVirtualSwitchOperationParams",
	"InsertSecurityGroupOperationParams",
	"InsertDefaultEgressOperationParams",
}

// TestOperationInsertsAlwaysCarryTelemetry is a regression guard for review
// finding P1: AcceptEIP's set_eip_security_groups sub-operation was inserted
// without TelemetryContext, so the SG worker's first claim always reported
// context_missing. Reading the call sites statically catches the missing
// argument at any entry point, including ones a fixture does not reach.
func TestOperationInsertsAlwaysCarryTelemetry(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "sqlcgen" {
				return true
			}
			guarded := false
			for _, want := range telemetryWriteGuard {
				if sel.Sel.Name == want {
					guarded = true
					break
				}
			}
			if !guarded {
				return true
			}
			checked++
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "TelemetryContext" {
					return true
				}
			}
			t.Errorf("%s: %s is written without TelemetryContext", fset.Position(lit.Pos()), sel.Sel.Name)
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no guarded insert call sites found; the guard would pass vacuously")
	}
}
