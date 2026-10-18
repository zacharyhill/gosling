package transpile

import (
	"go/ast"
	"slices"
)

// checkUsing reports a using statement whose Close cannot run at the right
// time, or whose error nothing checks.
func (lowerer *lowerer) checkUsing(stmt ast.Stmt, try *tryContext) {
	assign, ok := stmt.(*ast.AssignStmt)
	if !ok || !slices.Contains(lowerer.file.preprocessed.Usings, lowerer.offset(assign.Lhs[0].Pos())) {
		return
	}
	if lowerer.isInLoop(assign) {
		lowerer.errorf(lowerer.file, assign.Pos(), "using is not allowed in a loop, because Close runs only when the function returns; move the loop body into a function")
		return
	}
	if _, isFallible := fallible(lowerer.typeInfo, assign.Rhs[0]); isFallible && try.target() == nil {
		lowerer.errorf(lowerer.file, assign.Pos(), "using with a call that can fail must be in a try block")
	}
}

// isInLoop reports whether node is in a loop body of its own function.
func (lowerer *lowerer) isInLoop(node ast.Node) bool {
	for ancestor := lowerer.parents[node]; ancestor != nil; ancestor = lowerer.parents[ancestor] {
		switch ancestor.(type) {
		case *ast.ForStmt, *ast.RangeStmt:
			return true
		case *ast.FuncLit, *ast.FuncDecl:
			return false
		}
	}
	return false
}
