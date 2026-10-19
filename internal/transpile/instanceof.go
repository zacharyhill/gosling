package transpile

import (
	"go/ast"
	"go/types"
	"strings"
)

// recordNarrowings records the if statements with an instanceof condition,
// and the variables that they narrow.
func (file *file) recordNarrowings() {
	if len(file.preprocessed.Narrowings) == 0 {
		return
	}
	narrowedVarOffsets := map[int]bool{}
	for _, narrowing := range file.preprocessed.Narrowings {
		file.narrowings[narrowing.If] = narrowing
		narrowedVarOffsets[narrowing.Var] = true
	}
	ast.Inspect(file.ast, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok && narrowedVarOffsets[file.tokenFile.Offset(ident.Pos())] {
			file.narrowedVars[ident] = true
		}
		return true
	})
}

// isUnusedNarrowing reports whether typeError is "declared and not used"
// for a narrowed variable. Lowering replaces such a variable with _.
func (file *file) isUnusedNarrowing(typeError types.Error) bool {
	if !strings.HasPrefix(typeError.Msg, "declared and not used") {
		return false
	}
	for ident := range file.narrowedVars {
		if ident.Pos() == typeError.Pos {
			return true
		}
	}
	return false
}

// lowerNarrowing finishes the if statement that an instanceof condition
// became. An unused narrowed variable becomes _. For an error, the type
// assertion becomes errors.AsType, which also finds wrapped errors.
func (lowerer *lowerer) lowerNarrowing(stmt *ast.IfStmt, copyStart, copyEnd int) {
	parts, ok := findNarrowingParts(stmt, copyEnd > 0)
	if !ok {
		lowerer.errorf(lowerer.file, stmt.Pos(), "internal error: unexpected form of the if statement for instanceof")
		return
	}
	line := lowerer.line(stmt.Pos())
	switch {
	case parts.narrowed.Name != "_" && !lowerer.isUsed(parts.narrowed):
		lowerer.replace(parts.declared.Pos(), parts.declared.End(), "_", line)
		if copyEnd > 0 {
			lowerer.replaceOffsets(copyStart, copyEnd, "", line)
		}
	case copyEnd > 0:
		// The copy gets its own line, so the lines after it need a directive.
		copyText := strings.TrimSpace(string(lowerer.file.preprocessed.Src[copyStart:copyEnd]))
		lowerer.replaceOffsets(copyStart, copyEnd, "\n"+copyText, line)
	}
	if lowerer.isErrorAssertion(parts.assert) {
		lowerer.lowerAsType(parts.assert, line)
	}
}

// narrowingParts holds the nodes of `declared, ok := x.(T); ok`. The
// narrowed variable is declared, or with an else branch, the copy at the
// start of the body.
type narrowingParts struct {
	declared, narrowed *ast.Ident
	assert             *ast.TypeAssertExpr
}

// findNarrowingParts finds the nodes that preprocessing wrote for
// instanceof. It reports false if stmt does not have that form.
func findNarrowingParts(stmt *ast.IfStmt, hasCopy bool) (narrowingParts, bool) {
	assign, ok := stmt.Init.(*ast.AssignStmt)
	if !ok {
		return narrowingParts{}, false
	}
	assert, isAssert := assign.Rhs[0].(*ast.TypeAssertExpr)
	declared, isIdent := assign.Lhs[0].(*ast.Ident)
	if !isAssert || !isIdent {
		return narrowingParts{}, false
	}
	parts := narrowingParts{declared: declared, narrowed: declared, assert: assert}
	if !hasCopy {
		return parts, true
	}
	if len(stmt.Body.List) == 0 {
		return narrowingParts{}, false
	}
	copyAssign, ok := stmt.Body.List[0].(*ast.AssignStmt)
	if !ok {
		return narrowingParts{}, false
	}
	parts.narrowed, ok = copyAssign.Lhs[0].(*ast.Ident)
	return parts, ok
}

func (lowerer *lowerer) isUsed(ident *ast.Ident) bool {
	object := lowerer.typeInfo.Defs[ident]
	if lowerer.usedObjects == nil {
		lowerer.usedObjects = map[types.Object]bool{}
		for _, used := range lowerer.typeInfo.Uses {
			lowerer.usedObjects[used] = true
		}
	}
	return object == nil || lowerer.usedObjects[object]
}

// isErrorAssertion reports whether assert asserts an error to a type that
// errors.AsType accepts.
func (lowerer *lowerer) isErrorAssertion(assert *ast.TypeAssertExpr) bool {
	subjectType := lowerer.typeInfo.Types[assert.X].Type
	targetType := lowerer.typeInfo.Types[assert.Type].Type
	if subjectType == nil || targetType == nil {
		return false
	}
	return types.IsInterface(subjectType) && types.AssignableTo(subjectType, errorType) && types.AssignableTo(targetType, errorType)
}

// lowerAsType turns x.(T) into errors.AsType[T](x).
func (lowerer *lowerer) lowerAsType(assert *ast.TypeAssertExpr, line int) {
	errorsName, ok := lowerer.importNameAt(assert.X.Pos(), "errors", "errors")
	if !ok {
		lowerer.errorf(lowerer.file, assert.X.Pos(), "instanceof needs package errors, but the name errors is already used in this file")
		return
	}
	offset := lowerer.offset(assert.Pos())
	lowerer.edits = append(lowerer.edits, edit{
		start:  offset,
		end:    offset,
		line:   line,
		move:   &[2]int{lowerer.offset(assert.Type.Pos()), lowerer.offset(assert.Type.End())},
		before: qualify(errorsName) + "AsType[",
		after:  "](",
	})
	lowerer.drop(assert.X.End(), assert.End(), ")", line)
}
