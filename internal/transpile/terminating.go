package transpile

import (
	"go/ast"
	"go/token"
	"go/types"
)

// isTerminating reports whether stmt is a terminating statement as the Go
// spec defines it. It can return false for some terminating statements.
// The cost is an unneeded jump in the output. It never returns true for a
// statement that can complete normally.
func isTerminating(stmt ast.Stmt) bool { return terminates(stmt, isPanic) }

// terminates is isTerminating, with noReturn to decide which calls never return.
func terminates(stmt ast.Stmt, noReturn func(*ast.CallExpr) bool) bool {
	switch stmt := stmt.(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.BranchStmt:
		return stmt.Tok == token.GOTO
	case *ast.ExprStmt:
		call, ok := ast.Unparen(stmt.X).(*ast.CallExpr)
		return ok && noReturn(call)
	case *ast.BlockStmt:
		return terminatesList(stmt.List, noReturn)
	case *ast.IfStmt:
		return stmt.Else != nil && terminates(stmt.Body, noReturn) && terminates(stmt.Else, noReturn)
	case *ast.LabeledStmt:
		return terminates(stmt.Stmt, noReturn)
	case *ast.ForStmt:
		return stmt.Cond == nil && !hasBreak(stmt.Body)
	case *ast.SwitchStmt:
		return clausesTerminate(stmt.Body, true, noReturn)
	case *ast.TypeSwitchStmt:
		return clausesTerminate(stmt.Body, true, noReturn)
	case *ast.SelectStmt:
		return clausesTerminate(stmt.Body, false, noReturn)
	}
	return false
}

func isPanic(call *ast.CallExpr) bool {
	ident, ok := ast.Unparen(call.Fun).(*ast.Ident)
	return ok && ident.Name == "panic"
}

func terminatesList(list []ast.Stmt, noReturn func(*ast.CallExpr) bool) bool {
	if last := lastStmt(list); last != nil {
		return terminates(last, noReturn)
	}
	return false
}

// lastStmt returns the last non-empty statement in list, or nil.
func lastStmt(list []ast.Stmt) ast.Stmt {
	for stmtIndex := len(list) - 1; stmtIndex >= 0; stmtIndex-- {
		if _, empty := list[stmtIndex].(*ast.EmptyStmt); !empty {
			return list[stmtIndex]
		}
	}
	return nil
}

func clausesTerminate(body *ast.BlockStmt, needsDefault bool, noReturn func(*ast.CallExpr) bool) bool {
	hasDefault := false
	for _, clause := range body.List {
		var clauseBody []ast.Stmt
		switch clause := clause.(type) {
		case *ast.CaseClause:
			clauseBody = clause.Body
			hasDefault = hasDefault || clause.List == nil
		case *ast.CommClause:
			clauseBody = clause.Body
		}
		if !(terminatesList(clauseBody, noReturn) || endsInFallthrough(clauseBody)) || hasBreak(&ast.BlockStmt{List: clauseBody}) {
			return false
		}
	}
	return hasDefault || !needsDefault
}

// hasBreak reports whether node contains a break statement. It is
// conservative and also counts breaks that target nested statements.
func hasBreak(node ast.Node) bool {
	found := false
	ast.Inspect(node, func(node ast.Node) bool {
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		if branch, ok := node.(*ast.BranchStmt); ok && branch.Tok == token.BREAK {
			found = true
		}
		return !found
	})
	return found
}

func endsInFallthrough(list []ast.Stmt) bool {
	if len(list) == 0 {
		return false
	}
	branch, ok := list[len(list)-1].(*ast.BranchStmt)
	return ok && branch.Tok == token.FALLTHROUGH
}

// noReturnFuncs holds the functions and methods that never return normally,
// keyed by types.Func.FullName.
var noReturnFuncs = map[string]bool{
	"os.Exit":                   true,
	"runtime.Goexit":            true,
	"log.Fatal":                 true,
	"log.Fatalf":                true,
	"log.Fatalln":               true,
	"log.Panic":                 true,
	"log.Panicf":                true,
	"log.Panicln":               true,
	"(*log.Logger).Fatal":       true,
	"(*log.Logger).Fatalf":      true,
	"(*log.Logger).Fatalln":     true,
	"(*log.Logger).Panic":       true,
	"(*log.Logger).Panicf":      true,
	"(*log.Logger).Panicln":     true,
	"(*testing.common).Fatal":   true,
	"(*testing.common).Fatalf":  true,
	"(*testing.common).FailNow": true,
	"(*testing.common).Skip":    true,
	"(*testing.common).Skipf":   true,
	"(*testing.common).SkipNow": true,
	"(testing.TB).Fatal":        true,
	"(testing.TB).Fatalf":       true,
	"(testing.TB).FailNow":      true,
	"(testing.TB).Skip":         true,
	"(testing.TB).Skipf":        true,
	"(testing.TB).SkipNow":      true,
}

// neverReturns reports calls to panic and to noReturnFuncs.
func neverReturns(typeInfo *types.Info) func(*ast.CallExpr) bool {
	return func(call *ast.CallExpr) bool {
		var ident *ast.Ident
		switch callee := ast.Unparen(call.Fun).(type) {
		case *ast.Ident:
			ident = callee
		case *ast.SelectorExpr:
			ident = callee.Sel
		default:
			return false
		}
		switch object := typeInfo.Uses[ident].(type) {
		case *types.Builtin:
			return object.Name() == "panic"
		case *types.Func:
			return noReturnFuncs[object.FullName()]
		}
		return false
	}
}
