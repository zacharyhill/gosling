package transpile

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"
)

// checkCommaOK reports each comma-ok destructuring, such as
// const [a, b] = rows[0], whose right side has no comma-ok form. The
// preprocessor picks the form from the syntax, so a slice index ends up
// here. It returns the reported statements.
func (transpiler *transpiler) checkCommaOK(typeInfo *types.Info) []*ast.AssignStmt {
	var reported []*ast.AssignStmt
	for _, file := range transpiler.files {
		if len(file.preprocessed.CommaOK) == 0 {
			continue
		}
		ast.Inspect(file.ast, func(node ast.Node) bool {
			assign, ok := node.(*ast.AssignStmt)
			if !ok || len(assign.Rhs) != 1 || !slices.Contains(file.preprocessed.CommaOK, file.tokenFile.Offset(assign.Pos())) {
				return true
			}
			rightSide := typeInfo.Types[assign.Rhs[0]]
			if rightSide.Type == nil || rightSide.Type == types.Typ[types.Invalid] || rightSide.HasOk() {
				return true
			}
			transpiler.errorf(file, assign.Rhs[0].Pos(), "%s is not a map index, a type assertion, or a receive; to destructure the value, assign it to a variable first", file.text(assign.Rhs[0]))
			reported = append(reported, assign)
			return true
		})
	}
	return reported
}

func isInStatements(statements []*ast.AssignStmt, position token.Pos) bool {
	return slices.ContainsFunc(statements, func(statement *ast.AssignStmt) bool {
		return position >= statement.Pos() && position < statement.End()
	})
}
