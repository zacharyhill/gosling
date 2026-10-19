package transpile

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

// tupleDestructure is an array pattern that takes the results of a call.
// The preprocessor wrote `tmp := f(); a, b := tmp[0], tmp[1]`, which
// becomes `a, b := f()`.
type tupleDestructure struct {
	tempVar         *ast.Ident
	tempOperatorPos token.Pos
	bindingOperator token.Token
	names           string
	bindingEnd      token.Pos
}

// destructureTuples rewrites each array pattern whose right side is a call
// with more than one result besides an error. It reports whether anything
// changed.
func destructureTuples(file *file, typeInfo *types.Info) bool {
	if len(file.preprocessed.ArrayTemps) == 0 {
		return false
	}
	tempDecls := findTupleTempDecls(file, typeInfo)
	if len(tempDecls) == 0 {
		return false
	}
	changed := false
	astutil.Apply(file.ast, func(cursor *astutil.Cursor) bool {
		binding, ok := cursor.Node().(*ast.AssignStmt)
		if !ok || cursor.Index() < 0 {
			return true
		}
		tempDecl, ok := tempDecls[findBindingTemp(binding)]
		if !ok || len(tempDecl.elementNames) < 2 {
			return true
		}
		file.takeResults(tempDecl, binding)
		cursor.Delete()
		changed = true
		return false
	}, nil)
	return changed
}

// tupleTempDecl is the declaration of an array pattern temporary whose right
// side has more than one value.
type tupleTempDecl struct {
	assign       *ast.AssignStmt
	elementNames []string // with "_" for a blank element
}

// findTupleTempDecls returns the array pattern temporaries whose right side
// has more than one value, by name.
func findTupleTempDecls(file *file, typeInfo *types.Info) map[string]tupleTempDecl {
	namesByOffset := map[int][]string{}
	for _, arrayTemp := range file.preprocessed.ArrayTemps {
		namesByOffset[arrayTemp.Offset] = arrayTemp.Names
	}
	tempDecls := map[string]tupleTempDecl{}
	ast.Inspect(file.ast, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 || file.tuples[assign] != nil {
			return true
		}
		tempVar, ok := assign.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		elementNames, isArrayTemp := namesByOffset[file.tokenFile.Offset(tempVar.Pos())]
		if isArrayTemp && countValues(typeInfo, assign.Rhs[0]) > 1 {
			tempDecls[tempVar.Name] = tupleTempDecl{assign: assign, elementNames: elementNames}
		}
		return true
	})
	return tempDecls
}

// findBindingTemp returns the name of the temporary that binding reads, as in
// `a, b := tmp[0], tmp[1]`, or `_ = tmp` for a pattern of blank elements.
func findBindingTemp(binding *ast.AssignStmt) string {
	if len(binding.Rhs) == 0 {
		return ""
	}
	value := binding.Rhs[0]
	if indexExpr, ok := value.(*ast.IndexExpr); ok {
		value = indexExpr.X
	}
	tempVar, ok := value.(*ast.Ident)
	if !ok {
		return ""
	}
	return tempVar.Name
}

// takeResults moves the names of binding to the left side of tempDecl. The
// binding skips blank elements, so they get new blank names.
func (file *file) takeResults(tempDecl tupleTempDecl, binding *ast.AssignStmt) {
	tempVar := tempDecl.assign.Lhs[0].(*ast.Ident)
	file.tuples[tempDecl.assign] = &tupleDestructure{
		tempVar:         tempVar,
		tempOperatorPos: tempDecl.assign.TokPos,
		bindingOperator: binding.Tok,
		names:           strings.Join(tempDecl.elementNames, ", "),
		bindingEnd:      binding.End(),
	}
	boundNames := binding.Lhs
	if isBlankOnlyBinding(binding) {
		boundNames = nil
	}
	targets := make([]ast.Expr, len(tempDecl.elementNames))
	for nameIndex, elementName := range tempDecl.elementNames {
		if elementName == "_" || len(boundNames) == 0 {
			targets[nameIndex] = &ast.Ident{Name: "_"}
		} else {
			targets[nameIndex], boundNames = boundNames[0], boundNames[1:]
		}
		// The names now stand in the text of the temporary. The last name ends
		// where the temporary ends, so a check can append the error variable.
		name := targets[nameIndex].(*ast.Ident)
		name.NamePos = tempVar.End() - token.Pos(len(name.Name))
		if nameIndex == 0 {
			name.NamePos = tempVar.Pos()
		}
	}
	tempDecl.assign.Lhs = targets
	tempDecl.assign.Tok = binding.Tok
}

// isBlankOnlyBinding reports whether binding is the `_ = tmp` of a pattern
// with only blank elements.
func isBlankOnlyBinding(binding *ast.AssignStmt) bool {
	_, isIdent := binding.Rhs[0].(*ast.Ident)
	return isIdent
}

// countValues returns the number of results of the call expr without a last
// error result.
func countValues(typeInfo *types.Info, expr ast.Expr) int {
	tuple, ok := typeInfo.Types[expr].Type.(*types.Tuple)
	if !ok {
		return 0
	}
	if tuple.Len() > 0 && isErrorResult(tuple.At(tuple.Len()-1).Type()) {
		return tuple.Len() - 1
	}
	return tuple.Len()
}

// lowerTuples writes the patterns that destructureTuples changed.
func (lowerer *lowerer) lowerTuples() {
	for tempDecl, tuple := range lowerer.file.tuples {
		line := lowerer.line(tempDecl.Pos())
		lowerer.replace(tuple.tempVar.Pos(), tuple.tempVar.End(), tuple.names, line)
		if tuple.bindingOperator != token.DEFINE {
			operatorEnd := tuple.tempOperatorPos + token.Pos(len(token.DEFINE.String()))
			lowerer.replace(tuple.tempOperatorPos, operatorEnd, tuple.bindingOperator.String(), line)
		}
		lowerer.drop(tempDecl.End(), tuple.bindingEnd, "", line)
	}
}
