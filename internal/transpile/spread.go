package transpile

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strings"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// spreadMarkerDecls declares the spread markers for type checking.
var spreadMarkerDecls = "func " + preprocess.CatMarker + "[S ~[]E, E any](parts ...S) S { return nil }\n" +
	"func " + preprocess.OneMarker + "[E any](v ...E) []E { return v }\n" +
	"func " + preprocess.WithMarker + "[V any](base, update V) V { return base }\n" +
	"func " + preprocess.AddrMarker + "[V any](value V) *V { return &value }\n"

// expandShorthand changes T{name, age} into T{name: name, age: age} if the
// plain positional literal is not valid. This is the case if the literal
// has fewer values than the struct has fields. It is also the case if the
// literal mixes in keyed fields or updates a struct spread. It reports whether anything changed.
func (transpiler *transpiler) expandShorthand(file *file, typeInfo *types.Info) bool {
	changed := false
	parentOf := parents(file.ast)
	ast.Inspect(file.ast, func(node ast.Node) bool {
		compositeLit, ok := node.(*ast.CompositeLit)
		if !ok || len(compositeLit.Elts) == 0 {
			return true
		}
		structType, ok := underlyingStruct(typeInfo.Types[compositeLit].Type)
		if !ok {
			return true
		}
		inSpreadUpdate := isSpreadUpdate(parentOf, compositeLit)
		if !inSpreadUpdate && !hasKeyedElement(compositeLit) && len(compositeLit.Elts) >= structType.NumFields() {
			return true // a full positional literal keeps its Go meaning
		}
		changed = transpiler.keyShorthandFields(file, compositeLit, structType, inSpreadUpdate) || changed
		return true
	})
	return changed
}

func hasKeyedElement(compositeLit *ast.CompositeLit) bool {
	for _, element := range compositeLit.Elts {
		if _, ok := element.(*ast.KeyValueExpr); ok {
			return true
		}
	}
	return false
}

// keyShorthandFields changes each bare field name in compositeLit into a
// keyed element. It reports unnamed values after a struct spread.
func (transpiler *transpiler) keyShorthandFields(file *file, compositeLit *ast.CompositeLit, structType *types.Struct, inSpreadUpdate bool) bool {
	fields := map[string]bool{}
	for field := range structType.Fields() {
		fields[field.Name()] = true
	}
	changed := false
	for elementIndex, element := range compositeLit.Elts {
		ident, ok := element.(*ast.Ident)
		if !ok || !fields[ident.Name] {
			if _, isKeyValue := element.(*ast.KeyValueExpr); !isKeyValue && inSpreadUpdate && !file.badSpreads[compositeLit] {
				file.badSpreads[compositeLit] = true
				transpiler.errorf(file, element.Pos(), "fields after a struct spread must be named")
			}
			continue
		}
		keyValue := &ast.KeyValueExpr{Key: &ast.Ident{Name: ident.Name, NamePos: ident.Pos()}, Colon: ident.Pos(), Value: ident}
		compositeLit.Elts[elementIndex] = keyValue
		file.shorthand[keyValue] = ident
		changed = true
	}
	return changed
}

func isSpreadUpdate(parentOf map[ast.Node]ast.Node, compositeLit *ast.CompositeLit) bool {
	call, ok := parentOf[compositeLit].(*ast.CallExpr)
	if !ok || len(call.Args) != 2 || call.Args[1] != compositeLit {
		return false
	}
	ident, ok := call.Fun.(*ast.Ident)
	return ok && ident.Name == preprocess.WithMarker
}

// lowerSpread writes out struct shorthand, slice spreads, and struct spreads.
func (lowerer *lowerer) lowerSpread() {
	for keyValue, ident := range lowerer.file.shorthand {
		if compositeLit, ok := lowerer.parents[keyValue].(*ast.CompositeLit); ok && !isSpreadUpdate(lowerer.parents, compositeLit) {
			lowerer.insert(ident.Pos(), ident.Name+": ", lowerer.line(ident.Pos()))
		}
	}
	ast.Inspect(lowerer.file.ast, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		switch ident.Name {
		case preprocess.CatMarker:
			lowerer.lowerCat(call, ident)
		case preprocess.WithMarker:
			lowerer.lowerWith(call)
		}
		return true
	})
}

// lowerCat writes [...a, x, ...b] as slices.Concat(a, []T{x}, b).
func (lowerer *lowerer) lowerCat(call *ast.CallExpr, callee *ast.Ident) {
	line := lowerer.line(call.Pos())
	slice, ok := underlyingSlice(lowerer.typeInfo.Types[call].Type)
	if !ok {
		return // the type checker already reported the error
	}
	elementType, ok := lowerer.typeName(slice.Elem())
	slicesName, hasSlicesName := lowerer.importNameAt(call.Pos(), "slices", "slices")
	if !ok || !hasSlicesName {
		lowerer.errorf(lowerer.file, call.Pos(), "spread needs package slices and the element type, but a needed name is already used in this file")
		return
	}
	if slices.ContainsFunc(call.Args, isOneCall) && lowerer.reportHiddenType(slice.Elem(), elementType, call.Pos()) {
		return
	}
	lowerer.replace(callee.Pos(), callee.End(), qualify(slicesName)+"Concat", line)
	for _, arg := range call.Args {
		if oneCall, ok := matchOneCall(arg); ok {
			lowerer.replace(oneCall.Pos(), oneCall.Lparen+1, "[]"+elementType+"{", line)
			lowerer.replace(oneCall.Rparen, oneCall.Rparen+1, "}", line)
		}
	}
}

func isOneCall(arg ast.Expr) bool {
	_, ok := matchOneCall(arg)
	return ok
}

// typeSpreadConstants gives the constants in a slice spread the element type
// of the spread slices, so [...int64s, 1] works. Without this, Go gives them
// their default type. It reports whether anything changed.
func (transpiler *transpiler) typeSpreadConstants(file *file, typeInfo *types.Info) bool {
	changed := false
	ast.Inspect(file.ast, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if calleeIdent, ok := call.Fun.(*ast.Ident); !ok || calleeIdent.Name != preprocess.CatMarker {
			return true
		}
		elementType := findSpreadElementType(call, typeInfo)
		if elementType == nil {
			return true
		}
		for _, arg := range call.Args {
			oneCall, ok := callTo(arg, preprocess.OneMarker)
			if !ok || !shouldAddElementType(oneCall, elementType, typeInfo) {
				continue
			}
			if typeArg, ok := transpiler.typeExpr(file, elementType, oneCall.Pos()); ok {
				oneCall.Fun = &ast.IndexExpr{X: oneCall.Fun, Index: typeArg}
				changed = true
			}
		}
		return true
	})
	return changed
}

// findSpreadElementType returns the element type of the first spread slice in
// a slice spread call, or nil.
func findSpreadElementType(catCall *ast.CallExpr, typeInfo *types.Info) types.Type {
	for _, arg := range catCall.Args {
		if _, isOneCall := matchOneCall(arg); isOneCall {
			continue
		}
		if slice, ok := underlyingSlice(typeInfo.Types[arg].Type); ok {
			return slice.Elem()
		}
	}
	return nil
}

// shouldAddElementType reports whether oneCall has a constant and its inferred
// element type differs from elementType.
func shouldAddElementType(oneCall *ast.CallExpr, elementType types.Type, typeInfo *types.Info) bool {
	if slice, ok := underlyingSlice(typeInfo.Types[oneCall].Type); ok && types.Identical(slice.Elem(), elementType) {
		return false
	}
	for _, arg := range oneCall.Args {
		if typeInfo.Types[arg].Value != nil {
			return true
		}
	}
	return false
}

// matchOneCall returns the element run call in arg. typeSpreadConstants can
// add a type argument to its callee.
func matchOneCall(arg ast.Expr) (*ast.CallExpr, bool) {
	call, ok := arg.(*ast.CallExpr)
	if !ok {
		return nil, false
	}
	callee := call.Fun
	if indexExpr, ok := callee.(*ast.IndexExpr); ok {
		callee = indexExpr.X
	}
	calleeIdent, ok := callee.(*ast.Ident)
	return call, ok && calleeIdent.Name == preprocess.OneMarker
}

// lowerWith writes T(__goslingWith(base, T{X: 1})) as
// func() T { v := base; v.X = 1; return v }().
func (lowerer *lowerer) lowerWith(withCall *ast.CallExpr) {
	conversion, ok := lowerer.parents[withCall].(*ast.CallExpr)
	if !ok || len(withCall.Args) != 2 {
		return
	}
	compositeLit, ok := withCall.Args[1].(*ast.CompositeLit)
	if !ok {
		return
	}
	base := withCall.Args[0]
	if lowerer.file.badSpreads[compositeLit] {
		return
	}
	line := lowerer.line(conversion.Pos())
	typeText := strings.TrimSpace(lowerer.file.text(conversion.Fun))
	tempVar := lowerer.pickTempVar(conversion.Pos(), conversion.End())
	var replacedExpr ast.Expr = conversion
	returnText := tempVar
	if addrCall, isAddressed := lowerer.getAddrMarkerCall(conversion); isAddressed {
		replacedExpr, typeText, returnText = addrCall, "*"+typeText, "&"+tempVar
	}
	lowerer.replace(replacedExpr.Pos(), base.Pos(), lowerer.buildTextWithComments(replacedExpr.Pos(), base.Pos(), "func() "+typeText+" {", tempVar+" := "), line)
	previousEnd := base.End()
	for _, element := range compositeLit.Elts {
		keyValue := element.(*ast.KeyValueExpr)
		if ident, isShorthand := lowerer.file.shorthand[keyValue]; isShorthand {
			lowerer.replace(previousEnd, ident.Pos(), lowerer.buildTextWithComments(previousEnd, ident.Pos(), "", tempVar+"."+ident.Name+" = "), line)
		} else {
			lowerer.replace(previousEnd, keyValue.Key.Pos(), lowerer.buildTextWithComments(previousEnd, keyValue.Key.Pos(), "", tempVar+"."), line)
			lowerer.replace(keyValue.Key.End(), keyValue.Value.Pos(), " = ", line)
		}
		previousEnd = keyValue.Value.End()
	}
	lowerer.replace(previousEnd, replacedExpr.End(), lowerer.buildTextWithComments(previousEnd, replacedExpr.End(), "", "return "+returnText+"\n}()"), line)
}

func (lowerer *lowerer) getAddrMarkerCall(conversion *ast.CallExpr) (*ast.CallExpr, bool) {
	parentCall, isCall := lowerer.parents[conversion].(*ast.CallExpr)
	if !isCall {
		return nil, false
	}
	return callTo(parentCall, preprocess.AddrMarker)
}

// buildTextWithComments returns before, a newline, and after, with the comments in
// [from, to). A comment on the line of from stays there. Other comments get their own lines.
func (lowerer *lowerer) buildTextWithComments(from, to token.Pos, before, after string) string {
	var replacement strings.Builder
	replacement.WriteString(before)
	var ownLineComments strings.Builder
	for _, group := range lowerer.file.ast.Comments {
		for _, comment := range group.List {
			if comment.Pos() < from || comment.End() > to {
				continue
			}
			if lowerer.line(comment.Pos()) == lowerer.line(from) {
				replacement.WriteString(" " + comment.Text)
			} else {
				ownLineComments.WriteString(comment.Text + "\n")
			}
		}
	}
	replacement.WriteString("\n" + ownLineComments.String() + after)
	return replacement.String()
}

func (file *file) inBadSpread(position token.Pos) bool {
	for compositeLit := range file.badSpreads {
		if position >= compositeLit.Pos() && position < compositeLit.End() {
			return true
		}
	}
	return false
}
