package transpile

import (
	"go/ast"
	"go/token"
	"go/types"
	"regexp"
	"strings"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// A try expression lowers to a function literal. The literal returns the
// value of the call, or the fallback if the call fails:
//
//	try strconv.Atoi(s) catch 0
//
// becomes
//
//	func() int {
//		if v, err := strconv.Atoi(s); err == nil {
//			return v
//		}
//		return 0
//	}()
//
// v and err are visible only in the if statement. The call runs before
// they exist and the fallback is outside the if, so they hide no names.

// matchTryExpression returns node, its call, and its fallback if node is a try
// expression. The preprocessor writes it as __goslingTry(call)(fallback).
func matchTryExpression(node ast.Node) (outerCall *ast.CallExpr, call, fallback ast.Expr, ok bool) {
	outerCall, ok = node.(*ast.CallExpr)
	if !ok || len(outerCall.Args) != 1 {
		return nil, nil, nil, false
	}
	marker, ok := callTo(outerCall.Fun, preprocess.TryMarker)
	if !ok || len(marker.Args) != 1 {
		return nil, nil, nil, false
	}
	return outerCall, marker.Args[0], outerCall.Args[0], true
}

func (lowerer *lowerer) lowerTryExpressions() {
	ast.Inspect(lowerer.file.ast, func(node ast.Node) bool {
		outerCall, call, fallback, ok := matchTryExpression(node)
		if !ok {
			return true
		}
		resultType := lowerer.typeInfo.Types[outerCall].Type
		typeText, ok := lowerer.typeName(resultType)
		if !ok {
			lowerer.typeNameTakenError(outerCall.Pos(), "try expression", typeText)
			return true
		}
		line := lowerer.line(outerCall.Pos())
		lowerer.replace(outerCall.Pos(), call.Pos(), "func() "+typeText+" {\nif v, err := ", line)
		lowerer.replace(call.End(), fallback.Pos(), "; err == nil {\nreturn v\n}\nreturn ", line)
		lowerer.replace(fallback.End(), outerCall.End(), "\n}()", line)
		return true
	})
}

const errTryCall = "a try expression needs a call that returns a value and an error"

// tryFallbackRE matches the end of a type error about the fallback of a try
// expression. The type checker names the function as the marker call.
var tryFallbackRE = regexp.MustCompile(`in argument to ` + preprocess.TryMarker + `\(.*$`)

// isTryCallError reports whether typeError is about the results of the call
// in a try expression.
func isTryCallError(typeError types.Error) bool {
	return strings.Contains(typeError.Msg, " to "+preprocess.TryMarker) && !tryFallbackRE.MatchString(typeError.Msg)
}

// findTryCall returns the position of the call in the innermost try
// expression around position. The type checker reports errors in this
// call at the end of the call.
func (transpiler *transpiler) findTryCall(position token.Pos) token.Pos {
	file := transpiler.fileAt(position)
	if file == nil {
		return position
	}
	callPosition := position
	ast.Inspect(file.ast, func(node ast.Node) bool {
		if node == nil || position < node.Pos() || position > node.End() {
			return false
		}
		if _, call, _, ok := matchTryExpression(node); ok {
			callPosition = call.Pos()
		}
		return true
	})
	return callPosition
}
