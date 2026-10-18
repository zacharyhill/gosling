package transpile

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/ast/astutil"
)

// rewriteLengths replaces x.length with len(x) if len accepts x and x has no
// field or method named length. It reports whether anything changed.
func (transpiler *transpiler) rewriteLengths(file *file, typeInfo *types.Info) bool {
	changed := false
	astutil.Apply(file.ast, nil, func(cursor *astutil.Cursor) bool {
		selector, ok := cursor.Node().(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "length" || !transpiler.hasLength(typeInfo, selector.X) {
			return true
		}
		call := &ast.CallExpr{
			Fun:    &ast.Ident{Name: "len", NamePos: selector.X.Pos()},
			Lparen: selector.X.Pos(),
			Args:   []ast.Expr{selector.X},
			Rparen: selector.Sel.End() - 1,
		}
		file.lengths[call] = selector
		cursor.Replace(call)
		changed = true
		return true
	})
	return changed
}

func (transpiler *transpiler) hasLength(typeInfo *types.Info, expr ast.Expr) bool {
	typeAndValue, ok := typeInfo.Types[expr]
	if !ok || !typeAndValue.IsValue() || typeAndValue.Type == nil {
		return false
	}
	if object, _, _ := types.LookupFieldOrMethod(typeAndValue.Type, true, transpiler.checkedPackage, "length"); object != nil {
		return false
	}
	switch underlying := typeAndValue.Type.Underlying().(type) {
	case *types.Slice, *types.Array, *types.Map, *types.Chan:
		return true
	case *types.Basic:
		return underlying.Info()&types.IsString != 0
	case *types.Pointer:
		_, isArray := underlying.Elem().Underlying().(*types.Array)
		return isArray
	}
	return false
}

// lowerLengths writes len( before each x.length and ) in place of .length.
func (lowerer *lowerer) lowerLengths() {
	for call, selector := range lowerer.file.lengths {
		line := lowerer.line(selector.Pos())
		lowerer.insertOrdered(selector.X.Pos(), "len(", line, lowerer.depth(call))
		lowerer.replace(selector.X.End(), selector.Sel.End(), ")", line)
	}
}
