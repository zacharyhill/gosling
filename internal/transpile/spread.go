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
	"func " + preprocess.WithMarker + "[V any](parts ...V) V { return parts[0] }\n" +
	"func " + preprocess.KeysMarker + "[V any](v V) V { return v }\n" +
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

// isSpreadUpdate reports whether compositeLit holds keyed elements of a
// spread in braces.
func isSpreadUpdate(parentOf map[ast.Node]ast.Node, compositeLit *ast.CompositeLit) bool {
	call, ok := parentOf[compositeLit].(*ast.CallExpr)
	if !ok || len(call.Args) != 1 || call.Args[0] != compositeLit {
		return false
	}
	ident, ok := call.Fun.(*ast.Ident)
	return ok && ident.Name == preprocess.KeysMarker
}

// lowerSpread writes out struct shorthand, slice spreads, and spreads in braces.
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

// typeUntypedSpreads gives the keyed elements of {...a, k: v} the type of
// the first spread. It reports whether anything changed.
func (transpiler *transpiler) typeUntypedSpreads(file *file, typeInfo *types.Info) bool {
	changed := false
	ast.Inspect(file.ast, func(node ast.Node) bool {
		withCall, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := withCall.Fun.(*ast.Ident); !ok || ident.Name != preprocess.WithMarker {
			return true
		}
		spreadType := findFirstSpreadType(withCall, typeInfo)
		if spreadType == nil {
			return true
		}
		for _, part := range withCall.Args {
			compositeLit, ok := findUntypedKeys(part)
			if !ok {
				continue
			}
			if typeArg, ok := transpiler.typeExpr(file, spreadType, compositeLit.Lbrace); ok {
				compositeLit.Type = typeArg
				changed = true
			}
		}
		return true
	})
	return changed
}

func findFirstSpreadType(withCall *ast.CallExpr, typeInfo *types.Info) types.Type {
	for _, part := range withCall.Args {
		if _, isKeys := callTo(part, preprocess.KeysMarker); isKeys {
			continue
		}
		if spreadType := typeInfo.Types[part].Type; spreadType != nil && spreadType != types.Typ[types.Invalid] {
			return spreadType
		}
		return nil
	}
	return nil
}

// findUntypedKeys returns the keyed elements in part if they still have the
// type UntypedMarker.
func findUntypedKeys(part ast.Expr) (*ast.CompositeLit, bool) {
	compositeLit, ok := findKeysLit(part)
	if !ok {
		return nil, false
	}
	typeIdent, ok := compositeLit.Type.(*ast.Ident)
	return compositeLit, ok && typeIdent.Name == preprocess.UntypedMarker
}

// spreadSegment is an expression that a lowered spread keeps, with the
// text to write before and after it.
type spreadSegment struct {
	expr           ast.Expr
	prefix, suffix string
}

// lowerWith writes T{...a, X: 1} for a struct as
// func() T { v := a; v.X = 1; return v }(). It writes T{...a, k: x, ...b}
// for a map as func() T { v := T{}; maps.Copy(v, a); v[k] = x; maps.Copy(v, b); return v }().
func (lowerer *lowerer) lowerWith(withCall *ast.CallExpr) {
	spreadExpr := ast.Expr(withCall)
	conversion, isTyped := lowerer.findSpreadConversion(withCall)
	if isTyped {
		spreadExpr = conversion
	}
	addrCall, isAddressed := lowerer.getAddrMarkerCall(conversion, isTyped)
	if len(withCall.Args) == 0 || lowerer.hasBadSpread(withCall) {
		return
	}
	goType := lowerer.typeInfo.Types[spreadExpr].Type
	if goType == nil {
		return // the type checker already reported the error
	}
	typeText, ok := lowerer.typeName(goType)
	if isTyped {
		typeText, ok = strings.TrimSpace(lowerer.file.text(conversion.Fun)), true
	}
	if !ok {
		lowerer.typeNameTakenError(spreadExpr.Pos(), "spread", displayName(goType))
		return
	}
	tempVar := lowerer.pickTempVar(spreadExpr.Pos(), spreadExpr.End(), nil)
	header, segments, ok := lowerer.buildSpreadSegments(withCall, spreadExpr, goType, typeText, tempVar)
	if !ok {
		return
	}
	resultType, returnText := typeText, tempVar
	if isAddressed {
		spreadExpr, resultType, returnText = addrCall, "*"+typeText, "&"+tempVar
	}
	lowerer.writeSegments(spreadExpr, "func() "+resultType+" {", header, segments, "return "+returnText+"\n}()")
}

// buildSpreadSegments returns the segments of a struct or map spread. A map
// spread also gets a header that makes the new map.
func (lowerer *lowerer) buildSpreadSegments(withCall *ast.CallExpr, spreadExpr ast.Expr, goType types.Type, typeText, tempVar string) (string, []spreadSegment, bool) {
	switch goType.Underlying().(type) {
	case *types.Struct:
		segments, ok := lowerer.buildStructSegments(withCall, tempVar)
		return "", segments, ok
	case *types.Map:
		segments, ok := lowerer.buildMapSegments(withCall, tempVar)
		return tempVar + " := " + typeText + "{}", segments, ok
	default:
		lowerer.errorf(lowerer.file, spreadExpr.Pos(), "spread in braces needs a map or struct type; use [...a] for slices")
		return "", nil, false
	}
}

// writeSegments replaces the text of spreadExpr around the segments. The
// comments between the segments stay.
func (lowerer *lowerer) writeSegments(spreadExpr ast.Expr, opening, header string, segments []spreadSegment, closing string) {
	line := lowerer.line(spreadExpr.Pos())
	first := segments[0]
	lowerer.replace(spreadExpr.Pos(), first.expr.Pos(), lowerer.buildTextWithComments(spreadExpr.Pos(), first.expr.Pos(), opening, header+first.prefix), line)
	for segmentIndex := 1; segmentIndex < len(segments); segmentIndex++ {
		previous, current := segments[segmentIndex-1], segments[segmentIndex]
		between := previous.suffix + current.prefix
		if statementPrefix, isStatement := strings.CutPrefix(current.prefix, "\n"); isStatement {
			between = previous.suffix + lowerer.buildTextWithComments(previous.expr.End(), current.expr.Pos(), "", statementPrefix)
		}
		lowerer.replace(previous.expr.End(), current.expr.Pos(), between, line)
	}
	last := segments[len(segments)-1]
	lowerer.replace(last.expr.End(), spreadExpr.End(), last.suffix+lowerer.buildTextWithComments(last.expr.End(), spreadExpr.End(), "", closing), line)
}

// findSpreadConversion returns the T(...) call that preprocessing writes around
// a spread with a type. A spread without a type has no such call.
func (lowerer *lowerer) findSpreadConversion(withCall *ast.CallExpr) (*ast.CallExpr, bool) {
	conversion, ok := lowerer.parents[withCall].(*ast.CallExpr)
	if !ok || len(conversion.Args) != 1 || conversion.Args[0] != withCall {
		return nil, false
	}
	offset := lowerer.offset(conversion.Lparen)
	_, written := lowerer.file.preprocessed.OrigRange(offset, offset+1)
	return conversion, !written
}

func (lowerer *lowerer) hasBadSpread(withCall *ast.CallExpr) bool {
	for _, part := range withCall.Args {
		if compositeLit, ok := findKeysLit(part); ok && lowerer.file.badSpreads[compositeLit] {
			return true
		}
	}
	return false
}

func findKeysLit(part ast.Expr) (*ast.CompositeLit, bool) {
	keysCall, ok := callTo(part, preprocess.KeysMarker)
	if !ok || len(keysCall.Args) != 1 {
		return nil, false
	}
	compositeLit, ok := keysCall.Args[0].(*ast.CompositeLit)
	return compositeLit, ok
}

// buildStructSegments allows one spread, first, and then named fields.
func (lowerer *lowerer) buildStructSegments(withCall *ast.CallExpr, tempVar string) ([]spreadSegment, bool) {
	for partIndex, part := range withCall.Args {
		_, isKeys := findKeysLit(part)
		isMisplaced := partIndex > 0 && !isKeys || partIndex == 0 && isKeys
		if isMisplaced {
			misplaced := withCall.Args[min(max(partIndex, 1), len(withCall.Args)-1)]
			lowerer.errorf(lowerer.file, misplaced.Pos(), "struct spread must be the first element")
			return nil, false
		}
	}
	segments := []spreadSegment{{expr: withCall.Args[0], prefix: tempVar + " := "}}
	if len(withCall.Args) == 1 {
		return segments, true
	}
	compositeLit, _ := findKeysLit(withCall.Args[1])
	for _, element := range compositeLit.Elts {
		keyValue := element.(*ast.KeyValueExpr)
		if ident, isShorthand := lowerer.file.shorthand[keyValue]; isShorthand {
			segments = append(segments, spreadSegment{expr: ident, prefix: "\n" + tempVar + "." + ident.Name + " = "})
			continue
		}
		segments = append(segments,
			spreadSegment{expr: keyValue.Key, prefix: "\n" + tempVar + "."},
			spreadSegment{expr: keyValue.Value, prefix: " = "})
	}
	return segments, true
}

// buildMapSegments copies each spread and sets each key in source order, so
// later entries win.
func (lowerer *lowerer) buildMapSegments(withCall *ast.CallExpr, tempVar string) ([]spreadSegment, bool) {
	mapsName, ok := lowerer.importName(lowerer.file, "maps", "maps", false)
	if !ok {
		lowerer.errorf(lowerer.file, withCall.Pos(), "map spread needs package maps, but the name maps is already used in this file")
		return nil, false
	}
	var segments []spreadSegment
	for _, part := range withCall.Args {
		compositeLit, isKeys := findKeysLit(part)
		if !isKeys {
			segments = append(segments, spreadSegment{expr: part, prefix: "\n" + qualify(mapsName) + "Copy(" + tempVar + ", ", suffix: ")"})
			continue
		}
		buildKeySegments, ok := lowerer.buildKeySegments(compositeLit, tempVar)
		if !ok {
			return nil, false
		}
		segments = append(segments, buildKeySegments...)
	}
	return segments, true
}

func (lowerer *lowerer) buildKeySegments(compositeLit *ast.CompositeLit, tempVar string) ([]spreadSegment, bool) {
	var segments []spreadSegment
	for _, element := range compositeLit.Elts {
		keyValue, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return nil, false // the type checker already reported the missing key
		}
		keyType, keyOK := lowerer.formatElidedType(keyValue.Key)
		valueType, valueOK := lowerer.formatElidedType(keyValue.Value)
		if !keyOK || !valueOK {
			return nil, false
		}
		segments = append(segments,
			spreadSegment{expr: keyValue.Key, prefix: "\n" + tempVar + "[" + keyType, suffix: "]"},
			spreadSegment{expr: keyValue.Value, prefix: " = " + valueType})
	}
	return segments, true
}

// formatElidedType returns the type to write before a composite literal whose
// type the map literal elides, as in map[string]Point{"a": {1, 2}}.
func (lowerer *lowerer) formatElidedType(expr ast.Expr) (string, bool) {
	compositeLit, ok := expr.(*ast.CompositeLit)
	if !ok || compositeLit.Type != nil {
		return "", true
	}
	litType, prefix := lowerer.typeInfo.Types[compositeLit].Type, ""
	if pointer, isPointer := litType.(*types.Pointer); isPointer {
		litType, prefix = pointer.Elem(), "&"
	}
	typeText, ok := lowerer.typeName(litType)
	if !ok {
		lowerer.typeNameTakenError(expr.Pos(), "map spread", displayName(litType))
		return "", false
	}
	return prefix + typeText, true
}

// getAddrMarkerCall returns the AddrMarker call around the conversion of a
// typed spread.
func (lowerer *lowerer) getAddrMarkerCall(conversion *ast.CallExpr, isTyped bool) (*ast.CallExpr, bool) {
	if !isTyped {
		return nil, false
	}
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
		if position >= compositeLit.Lbrace && position < compositeLit.End() {
			return true
		}
	}
	return false
}
