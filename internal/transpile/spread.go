package transpile

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// spreadMarkerDecls declares the spread markers for type checking.
var spreadMarkerDecls = "func " + preprocess.CatMarker + "[S ~[]E, E any](parts ...S) S { return nil }\n" +
	"func " + preprocess.OneMarker + "[E any](v ...E) []E { return v }\n" +
	"func " + preprocess.WithMarker + "[V any](base, update V) V { return base }\n"

// expandShorthand changes T{name, age} into T{name: name, age: age} if the
// plain positional literal is not valid. This is the case if the literal
// has fewer values than the struct has fields, mixes in keyed fields, or
// updates a struct spread. It reports whether anything changed.
func (transpiler *transpiler) expandShorthand(file *file, info *types.Info) bool {
	changed := false
	parentOf := parents(file.ast)
	ast.Inspect(file.ast, func(node ast.Node) bool {
		compositeLit, ok := node.(*ast.CompositeLit)
		if !ok || len(compositeLit.Elts) == 0 {
			return true
		}
		structType, ok := underlyingStruct(info.Types[compositeLit].Type)
		if !ok {
			return true
		}
		spreadUpdate := isSpreadUpdate(parentOf, compositeLit)
		if !spreadUpdate && !hasKeyedElement(compositeLit) && len(compositeLit.Elts) >= structType.NumFields() {
			return true // a full positional literal keeps its Go meaning
		}
		changed = transpiler.keyShorthandFields(file, compositeLit, structType, spreadUpdate) || changed
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
func (transpiler *transpiler) keyShorthandFields(file *file, compositeLit *ast.CompositeLit, structType *types.Struct, spreadUpdate bool) bool {
	fields := map[string]bool{}
	for field := range structType.Fields() {
		fields[field.Name()] = true
	}
	changed := false
	for elementIndex, element := range compositeLit.Elts {
		ident, ok := element.(*ast.Ident)
		if !ok || !fields[ident.Name] {
			if _, isKeyValue := element.(*ast.KeyValueExpr); !isKeyValue && spreadUpdate && !file.badSpreads[compositeLit] {
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
	slice, ok := underlyingSlice(lowerer.info.Types[call].Type)
	if !ok {
		return // the type checker already reported the error
	}
	elementType, ok := lowerer.typeName(slice.Elem())
	pkg, pkgFound := lowerer.importName(lowerer.file, "slices", "slices", false)
	if !ok || !pkgFound {
		lowerer.errorf(lowerer.file, call.Pos(), "spread needs package slices and the element type, but a needed name is already used in this file")
		return
	}
	lowerer.replace(callee.Pos(), callee.End(), qualify(pkg)+"Concat", line)
	for _, arg := range call.Args {
		if oneCall, ok := callTo(arg, preprocess.OneMarker); ok {
			lowerer.replace(oneCall.Pos(), oneCall.Lparen+1, "[]"+elementType+"{", line)
			lowerer.replace(oneCall.Rparen, oneCall.Rparen+1, "}", line)
		}
	}
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
	lowerer.replace(conversion.Pos(), base.Pos(), "func() "+typeText+" {\n"+tempVar+" := ", line)
	prevEnd := base.End()
	for _, element := range compositeLit.Elts {
		keyValue := element.(*ast.KeyValueExpr)
		if ident, isShorthand := lowerer.file.shorthand[keyValue]; isShorthand {
			lowerer.replace(prevEnd, ident.Pos(), "\n"+tempVar+"."+ident.Name+" = ", line)
		} else {
			lowerer.replace(prevEnd, keyValue.Key.Pos(), "\n"+tempVar+".", line)
			lowerer.replace(keyValue.Key.End(), keyValue.Value.Pos(), " = ", line)
		}
		prevEnd = keyValue.Value.End()
	}
	lowerer.replace(prevEnd, conversion.End(), "\nreturn "+tempVar+"\n}()", line)
}

func (file *file) inBadSpread(pos token.Pos) bool {
	for compositeLit := range file.badSpreads {
		if pos >= compositeLit.Pos() && pos < compositeLit.End() {
			return true
		}
	}
	return false
}
