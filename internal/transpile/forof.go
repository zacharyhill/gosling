package transpile

import (
	"go/ast"
	"go/types"
)

// recordForOfBlanks maps each range statement to the "_" key that its
// for-of loop added.
func (file *file) recordForOfBlanks() {
	if len(file.preprocessed.ForOfBlanks) == 0 {
		return
	}
	offsets := map[int]bool{}
	for _, offset := range file.preprocessed.ForOfBlanks {
		offsets[offset] = true
	}
	ast.Inspect(file.ast, func(node ast.Node) bool {
		rangeStmt, ok := node.(*ast.RangeStmt)
		if !ok {
			return true
		}
		if blank, ok := rangeStmt.Key.(*ast.Ident); ok && offsets[file.tokenFile.Offset(blank.Pos())] {
			file.forOfBlanks[rangeStmt] = blank
		}
		return true
	})
}

// dropForOfBlanks makes the for-of variable the key of a range that allows
// only one iteration variable. It reports whether the AST changed.
func dropForOfBlanks(file *file, typeInfo *types.Info) bool {
	changed := false
	for rangeStmt, blank := range file.forOfBlanks {
		if rangeStmt.Key != blank || rangeStmt.Value == nil || !isSingleVarRange(typeInfo.TypeOf(rangeStmt.X)) {
			continue
		}
		rangeStmt.Key, rangeStmt.Value = rangeStmt.Value, nil
		changed = true
	}
	return changed
}

// isSingleVarRange reports whether a range over rangedType gives only one
// iteration variable: a channel, an integer, or an iter.Seq function.
func isSingleVarRange(rangedType types.Type) bool {
	if rangedType == nil {
		return false
	}
	switch underlying := rangedType.Underlying().(type) {
	case *types.Chan:
		return true
	case *types.Basic:
		return underlying.Info()&types.IsInteger != 0
	case *types.Signature:
		if underlying.Params().Len() != 1 {
			return false
		}
		yield, ok := underlying.Params().At(0).Type().Underlying().(*types.Signature)
		return ok && yield.Params().Len() == 1
	}
	return false
}

// lowerForOfBlanks removes the "_, " of each range that dropForOfBlanks
// changed.
func (lowerer *lowerer) lowerForOfBlanks() {
	for rangeStmt, blank := range lowerer.file.forOfBlanks {
		if rangeStmt.Key != blank {
			lowerer.replace(blank.Pos(), rangeStmt.Key.Pos(), "", 0)
		}
	}
}
