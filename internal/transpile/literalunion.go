package transpile

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"strings"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// collectLiteralUnion records the constants of a literal union.
func (transpiler *transpiler) collectLiteralUnion(union preprocess.Union) {
	scope := transpiler.checkedPackage.Scope()
	unionType, ok := scope.Lookup(union.Name).(*types.TypeName)
	if !ok {
		return
	}
	var constants []*types.Const
	for _, name := range union.Constants {
		if unionConstant, ok := scope.Lookup(name).(*types.Const); ok {
			constants = append(constants, unionConstant)
		}
	}
	transpiler.literalUnions[unionType] = constants
}

// lookupLiteralUnion returns the constants of the literal union goType, if it
// is one.
func (lowerer *lowerer) lookupLiteralUnion(goType types.Type) (*types.Named, []*types.Const, bool) {
	named, ok := types.Unalias(goType).(*types.Named)
	if !ok {
		return nil, nil, false
	}
	constants, ok := lowerer.literalUnions[named.Obj()]
	return named, constants, ok
}

// checkLiteralSwitch reports a switch on a literal union that does not list
// each value and has no default case. Case values that are not constant do
// not count.
func (lowerer *lowerer) checkLiteralSwitch(stmt *ast.SwitchStmt) {
	if stmt.Tag == nil {
		return
	}
	union, constants, ok := lowerer.lookupLiteralUnion(lowerer.typeInfo.Types[stmt.Tag].Type)
	if !ok {
		return
	}
	var covered []constant.Value
	for _, clauseStmt := range stmt.Body.List {
		clause := clauseStmt.(*ast.CaseClause)
		if clause.List == nil {
			return
		}
		for _, expr := range clause.List {
			if value := lowerer.typeInfo.Types[expr].Value; value != nil {
				covered = append(covered, value)
			}
		}
	}
	var missing []string
	for _, unionConstant := range constants {
		if !containsConstant(covered, unionConstant.Val()) {
			missing = append(missing, unionConstant.Name())
		}
	}
	if len(missing) > 0 {
		lowerer.errorf(lowerer.file, stmt.Pos(), "switch on %s is missing cases: %s", union.Obj().Name(), strings.Join(missing, ", "))
	}
}

// checkLiteralConstants reports each constant expression of a literal union
// type whose value is not in the union, such as "PUT" in
// `let m: Method = "PUT"`.
func (lowerer *lowerer) checkLiteralConstants() {
	ast.Inspect(lowerer.file.ast, func(node ast.Node) bool {
		expr, ok := node.(ast.Expr)
		if !ok {
			return true
		}
		typeAndValue := lowerer.typeInfo.Types[expr]
		if typeAndValue.Value == nil {
			return true
		}
		union, constants, ok := lowerer.lookupLiteralUnion(typeAndValue.Type)
		if !ok {
			return true
		}
		values := make([]constant.Value, len(constants))
		for constantIndex, unionConstant := range constants {
			values[constantIndex] = unionConstant.Val()
		}
		if !containsConstant(values, typeAndValue.Value) {
			lowerer.errorf(lowerer.file, expr.Pos(), "%s is not a value of %s", typeAndValue.Value.ExactString(), union.Obj().Name())
		}
		return false
	})
}

func containsConstant(values []constant.Value, value constant.Value) bool {
	for _, candidate := range values {
		if candidate.Kind() == value.Kind() && constant.Compare(candidate, token.EQL, value) {
			return true
		}
	}
	return false
}
