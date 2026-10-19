package transpile

import (
	"go/ast"
	"go/types"
	"strings"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// getThrownValue returns the value of a throw statement, which the
// preprocessor writes as panic(__goslingThrow(value)).
func getThrownValue(stmt ast.Stmt) (ast.Expr, bool) {
	exprStmt, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return nil, false
	}
	panicCall, ok := callTo(exprStmt.X, "panic")
	if !ok || len(panicCall.Args) != 1 {
		return nil, false
	}
	marker, ok := callTo(panicCall.Args[0], preprocess.ThrowMarker)
	if !ok || len(marker.Args) != 1 {
		return nil, false
	}
	return marker.Args[0], true
}

// lowerThrow lowers a throw statement. Inside a try block, the value goes
// to the catch block. Outside, the function returns it. It reports whether
// stmt is a throw statement.
func (lowerer *lowerer) lowerThrow(stmt ast.Stmt, try *tryContext) bool {
	value, ok := getThrownValue(stmt)
	if !ok {
		return false
	}
	line := lowerer.line(stmt.Pos())
	if target := try.target(); target != nil {
		target.jumps++
		destination := target.catchVar
		if destination == "" {
			destination = "_" // a catch without a variable still evaluates the value
		}
		lowerer.replace(stmt.Pos(), value.Pos(), destination+" = ", line)
		lowerer.replace(value.End(), stmt.End(), "", line)
		lowerer.insertAfter(stmt, "", "\n"+lowerer.jumpOut(try, target, "goto "+target.catchLabel()), line)
		return true
	}
	signature := enclosingSignature(lowerer.typeInfo, lowerer.parents, stmt)
	if signature == nil || !returnsError(signature) {
		lowerer.errorf(lowerer.file, stmt.Pos(), "throw needs an enclosing try or a function whose last result is error")
		return true
	}
	zeros, ok := lowerer.zeroResults(signature, stmt)
	if !ok {
		return true
	}
	lowerer.replace(value.End(), stmt.End(), "", line)
	if layers := lowerer.crossed(stmt, try); len(layers) > 0 {
		lowerer.returnThroughFinally(stmt, layers, signature, value.Pos(), zeros)
		return true
	}
	lowerer.replace(stmt.Pos(), value.Pos(), "return "+zeros, line)
	return true
}

func returnsError(signature *types.Signature) bool {
	results := signature.Results()
	return results.Len() > 0 && types.Identical(results.At(results.Len()-1).Type(), errorType)
}

// zeroResults returns the zero values of all results but the last, each
// followed by ", ".
func (lowerer *lowerer) zeroResults(signature *types.Signature, stmt ast.Stmt) (string, bool) {
	var zeros strings.Builder
	for resultIndex := range signature.Results().Len() - 1 {
		resultType := signature.Results().At(resultIndex).Type()
		typeText, ok := lowerer.typeName(resultType)
		if !ok {
			lowerer.typeNameTakenError(stmt.Pos(), "throw", typeText)
			return "", false
		}
		zeros.WriteString(zeroValue(resultType, typeText) + ", ")
	}
	return zeros.String(), true
}
