package transpile

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"sort"
	"strings"
)

// Optional chains and ?? lower to nested ifs, one per link. Each if binds a
// temporary, so each link is evaluated once:
//
//	user?.Profile?.Name ?? "anon"
//
// becomes
//
//	func() string {
//		if v := user; v != nil {
//			if v := v.Profile; v != nil {
//				return v.Name
//			}
//		}
//		return "anon"
//	}()
//
// A map index binds the comma-ok result, and a slice index binds the index:
//
//	if v, ok := m[k]; ok {
//	if v, i := xs, n; 0 <= i && i < len(v) {
//
// Each piece of source text appears once, in its original order. So only
// the text between pieces changes, and edits inside the pieces survive.

// findChains records the parent of each node and the root of each optional
// chain. The root is the outermost selector, call, or index expression
// that continues the chain.
func (lowerer *lowerer) findChains() {
	lowerer.parents = parents(lowerer.file.ast)
	lowerer.optionalRoots = map[ast.Expr]bool{}
	for link := range lowerer.file.optional {
		root := link
		for {
			parent, ok := lowerer.parents[root].(ast.Expr)
			if !ok || spineChild(parent) != root {
				break
			}
			root = parent
		}
		lowerer.optionalRoots[root] = true
	}
}

// spineChild returns the operand a postfix expression applies to.
func spineChild(node ast.Node) ast.Expr {
	switch node := node.(type) {
	case *ast.SelectorExpr:
		return node.X
	case *ast.CallExpr:
		return node.Fun
	case *ast.IndexExpr:
		return node.X
	case *ast.IndexListExpr:
		return node.X
	case *ast.SliceExpr:
		return node.X
	case *ast.TypeAssertExpr:
		return node.X
	}
	return nil
}

// links returns the optional links of the chain that ends at root, in
// source order. A link is a selector, an index expression, or a call. It
// reports an error if the left side of a link has a type that ?. cannot guard.
func (lowerer *lowerer) links(root ast.Expr) ([]ast.Expr, bool) {
	links := lowerer.collectOptionalLinks(root)
	link := lowerer.findNonNilableLink(links)
	if link == nil {
		return links, true
	}
	left := spineChild(link)
	leftType := lowerer.typeInfo.Types[left].Type
	_, isIndex := link.(*ast.IndexExpr)
	_, isTypeParam := leftType.(*types.TypeParam)
	switch {
	case isIndex && !isTypeParam:
		lowerer.errorf(lowerer.file, left.End(), "?.[ needs a map, a slice, or a pointer to an array on its left, not %s", displayName(leftType))
	case isTypeParam:
		lowerer.errorf(lowerer.file, left.End(), "?. needs a pointer or interface on its left, but type parameter %s allows types that cannot be nil", displayName(leftType))
	default:
		lowerer.errorf(lowerer.file, left.End(), "?. needs a pointer or interface on its left, not %s", displayName(leftType))
	}
	return nil, false
}

// collectOptionalLinks returns the optional links of the chain that ends at
// root, in source order.
func (lowerer *lowerer) collectOptionalLinks(root ast.Expr) []ast.Expr {
	var links []ast.Expr
	for expr := root; expr != nil; expr = spineChild(expr) {
		if lowerer.file.optional[expr] {
			links = append(links, expr)
		}
	}
	slices.Reverse(links)
	return links
}

// findNonNilableLink returns the first link whose left side ?. cannot guard, or nil.
func (lowerer *lowerer) findNonNilableLink(links []ast.Expr) ast.Expr {
	for _, link := range links {
		left := spineChild(link)
		leftType := lowerer.typeInfo.Types[left].Type
		if _, isIndex := link.(*ast.IndexExpr); isIndex && getIndexKind(leftType) == notIndexable {
			return link
		}
		if !isComparableToNil(leftType) {
			return link
		}
	}
	return nil
}

type indexKind int

const (
	notIndexable indexKind = iota
	mapIndex
	sliceIndex
	arrayPointerIndex
)

func getIndexKind(goType types.Type) indexKind {
	if _, isTypeParam := goType.(*types.TypeParam); isTypeParam || goType == nil {
		return notIndexable
	}
	switch underlying := goType.Underlying().(type) {
	case *types.Map:
		return mapIndex
	case *types.Slice:
		return sliceIndex
	case *types.Pointer:
		if _, isArray := underlying.Elem().Underlying().(*types.Array); isArray {
			return arrayPointerIndex
		}
	}
	return notIndexable
}

// hasBareCompositeLit reports whether expr has a composite literal outside
// parentheses, brackets, or braces.
func hasBareCompositeLit(expr ast.Expr) bool {
	switch expr := expr.(type) {
	case *ast.CompositeLit:
		return true
	case *ast.SelectorExpr:
		return hasBareCompositeLit(expr.X)
	case *ast.CallExpr:
		return hasBareCompositeLit(expr.Fun)
	case *ast.IndexExpr:
		return hasBareCompositeLit(expr.X)
	case *ast.StarExpr:
		return hasBareCompositeLit(expr.X)
	case *ast.UnaryExpr:
		return hasBareCompositeLit(expr.X)
	case *ast.BinaryExpr:
		return hasBareCompositeLit(expr.X) || hasBareCompositeLit(expr.Y)
	}
	return false
}

func (lowerer *lowerer) lowerNullish() {
	calls := make([]*ast.CallExpr, 0, len(lowerer.file.coalesce))
	for call := range lowerer.file.coalesce {
		calls = append(calls, call)
	}
	// An outer ?? starts at the same offset as its left operand. Lower it
	// first, so its opening text goes before the opening text of the inner ??.
	sort.Slice(calls, func(left, right int) bool {
		if calls[left].Pos() != calls[right].Pos() {
			return calls[left].Pos() < calls[right].Pos()
		}
		return calls[left].End() > calls[right].End()
	})
	for _, call := range calls {
		lowerer.lowerCoalesce(call)
	}
	roots := make([]ast.Expr, 0, len(lowerer.optionalRoots))
	for root := range lowerer.optionalRoots {
		if !lowerer.coalesceOperand(root) {
			roots = append(roots, root)
		}
	}
	sort.Slice(roots, func(left, right int) bool { return roots[left].Pos() < roots[right].Pos() })
	for _, root := range roots {
		lowerer.lowerChain(root)
	}
	for assign := range lowerer.file.nullishAssigns {
		lowerer.lowerNullishAssign(assign)
	}
}

const errNullishAssignNotStatement = "??= can only be used as a statement in a block"

// lowerNullishAssign lowers x ??= d to an if statement. The if binds the
// address of x, or the map and key of a map element, so it evaluates x once:
//
//	if v := &x; *v == nil {
//		*v = d
//	}
//
//	if m, k := m, key; m[k] == nil {
//		m[k] = d
//	}
func (lowerer *lowerer) lowerNullishAssign(assign *ast.AssignStmt) {
	if !lowerer.checkNullishAssign(assign) {
		return
	}
	left, value := assign.Lhs[0], assign.Rhs[0]
	line, depth := lowerer.line(assign.Pos()), lowerer.depth(assign)
	lowerer.insertOrdered(assign.End(), "\n}", line, -depth)
	if indexExpr, ok := left.(*ast.IndexExpr); ok && lowerer.isMap(indexExpr.X) {
		mapVar, keyVar := lowerer.pickName("m", assign.Pos(), assign.End()), lowerer.pickName("k", assign.Pos(), assign.End())
		lowerer.insertIfHeader(assign.Pos(), indexExpr.X, fmt.Sprintf("if %s, %s := ", mapVar, keyVar), line, depth)
		lowerer.replace(indexExpr.X.End(), indexExpr.Lbrack+1, ", ", line)
		lowerer.replace(indexExpr.Rbrack, value.Pos(), fmt.Sprintf("; %s[%s] == nil {\n%s[%s] = ", mapVar, keyVar, mapVar, keyVar), line)
		return
	}
	tempVar := lowerer.pickTempVar(assign.Pos(), assign.End(), nil)
	lowerer.insertIfHeader(assign.Pos(), left, fmt.Sprintf("if %s := &", tempVar), line, depth)
	lowerer.replace(left.End(), value.Pos(), fmt.Sprintf("; *%s == nil {\n*%s = ", tempVar, tempVar), line)
}

// insertIfHeader inserts header at position, before operand. A composite
// literal in an if header needs parentheses.
func (lowerer *lowerer) insertIfHeader(position token.Pos, operand ast.Expr, header string, line, depth int) {
	if !hasBareCompositeLit(operand) {
		lowerer.insertOrdered(position, header, line, depth)
		return
	}
	lowerer.insertOrdered(position, header+"(", line, depth)
	lowerer.insertOrdered(operand.End(), ")", line, -depth)
}

// checkNullishAssign reports an error and false if ??= cannot lower assign.
func (lowerer *lowerer) checkNullishAssign(assign *ast.AssignStmt) bool {
	fail := func(format string, arguments ...any) bool {
		lowerer.errorf(lowerer.file, assign.Pos(), format, arguments...)
		return false
	}
	if len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return fail("??= needs one value on each side")
	}
	switch parent := lowerer.parents[assign].(type) {
	case *ast.BlockStmt, *ast.CaseClause, *ast.LabeledStmt:
	case *ast.CommClause:
		if parent.Comm == assign {
			return fail(errNullishAssignNotStatement)
		}
	default:
		return fail(errNullishAssignNotStatement)
	}
	left := assign.Lhs[0]
	switch {
	case isBlank(left):
		return fail("??= cannot assign to _")
	case ast.Unparen(left) != left:
		return fail("the left side of ??= cannot be in parentheses")
	case lowerer.optionalRoots[left]:
		return fail("the left side of ??= cannot use ?.")
	}
	leftType := lowerer.typeInfo.Types[left].Type
	if _, isTypeParam := leftType.(*types.TypeParam); isTypeParam || !nilable(leftType) {
		return fail("left side of ??= must be a pointer, map, slice, interface, func, or chan, not %s", displayName(leftType))
	}
	return true
}

func (lowerer *lowerer) isMap(expr ast.Expr) bool {
	goType := lowerer.typeInfo.Types[expr].Type
	if goType == nil {
		return false
	}
	_, isMap := goType.Underlying().(*types.Map)
	return isMap
}

func isBlank(expr ast.Expr) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "_"
}

// coalesceOperand reports whether the lowering of a ?? chain includes operand.
// This is true for a left side, or for the right side of a ?? that is a left side.
func (lowerer *lowerer) coalesceOperand(operand ast.Expr) bool {
	call, ok := lowerer.parents[operand].(*ast.CallExpr)
	if !ok || lowerer.file.coalesce[call] == nil {
		return false
	}
	return call.Args[0] == operand || lowerer.coalesceOperand(call)
}

func (lowerer *lowerer) parentExpr(expr ast.Expr) ast.Expr {
	parent, _ := lowerer.parents[expr].(ast.Expr)
	return parent
}

// operands flattens a ?? b ?? c into [a, b, c].
func (lowerer *lowerer) operands(call *ast.CallExpr) []ast.Expr {
	if inner, ok := call.Args[0].(*ast.CallExpr); ok && lowerer.file.coalesce[inner] != nil {
		return append(lowerer.operands(inner), call.Args[1])
	}
	return []ast.Expr{call.Args[0], call.Args[1]}
}

// guards emits the opening nested ifs for the chain from expr through its
// optional links. The last link gets lastPrefix instead of a guard. It
// returns the closing braces.
func (lowerer *lowerer) guards(expr ast.Expr, open string, links []ast.Expr, tempVar, lastPrefix string) string {
	start, line := expr.Pos(), lowerer.line(expr.Pos())
	names := guardNames{
		value: tempVar,
		ok:    lowerer.pickName("ok", expr.Pos(), expr.End()),
		index: lowerer.pickName("i", expr.Pos(), expr.End()),
	}
	base, header := expr, names.formatHeader(notIndexable)
	if len(links) > 0 {
		base, header = spineChild(links[0]), names.formatHeader(lowerer.getLinkIndexKind(links[0]))
	}
	if hasBareCompositeLit(base) {
		// A composite literal in an if header needs parentheses.
		lowerer.insertOrdered(start, open+header+"(", line, lowerer.depth(expr))
		lowerer.insertOrdered(base.End(), ")", line, -lowerer.depth(expr))
	} else {
		lowerer.insertOrdered(start, open+header, line, lowerer.depth(expr))
	}
	for linkIndex, link := range links {
		next := lastPrefix
		if linkIndex < len(links)-1 {
			next = names.formatHeader(lowerer.getLinkIndexKind(links[linkIndex+1]))
		}
		lowerer.guardLink(link, names, next+tempVar, line)
	}
	return strings.Repeat("\n}", len(links))
}

// guardNames holds the temporaries of one chain: the value of each link, the
// comma-ok result of a map index, and the index of a slice index.
type guardNames struct {
	value, ok, index string
}

// formatHeader starts the if statement that binds the left side of a link.
func (names guardNames) formatHeader(kind indexKind) string {
	switch kind {
	case mapIndex:
		return fmt.Sprintf("if %s, %s := ", names.value, names.ok)
	case sliceIndex, arrayPointerIndex:
		return fmt.Sprintf("if %s, %s := ", names.value, names.index)
	}
	return fmt.Sprintf("if %s := ", names.value)
}

func (lowerer *lowerer) getLinkIndexKind(link ast.Expr) indexKind {
	if indexExpr, ok := link.(*ast.IndexExpr); ok {
		return getIndexKind(lowerer.typeInfo.Types[indexExpr.X].Type)
	}
	return notIndexable
}

// guardLink ends the if header for link and starts its body with next,
// which is the start of the next header or of the last expression.
func (lowerer *lowerer) guardLink(link ast.Expr, names guardNames, next string, line int) {
	value := names.value
	switch link := link.(type) {
	case *ast.SelectorExpr:
		lowerer.replace(link.X.End(), link.Sel.Pos(), fmt.Sprintf("; %s != nil {\n%s.", value, next), line)
	case *ast.CallExpr:
		lowerer.replace(link.Fun.End(), link.Lparen, fmt.Sprintf("; %s != nil {\n%s", value, next), line)
	case *ast.IndexExpr:
		lowerer.guardIndexLink(link, names, next, line)
	}
}

// guardIndexLink ends the if header for an index link. A map index checks
// the comma-ok result. Other indexes bind the index once and check it
// against the length.
func (lowerer *lowerer) guardIndexLink(link *ast.IndexExpr, names guardNames, next string, line int) {
	kind := lowerer.getLinkIndexKind(link)
	if kind == mapIndex {
		lowerer.replace(link.X.End(), link.Lbrack, "", line)
		lowerer.replace(link.Rbrack, link.Rbrack+1, fmt.Sprintf("]; %s {\n%s", names.ok, next), line)
		return
	}
	open, closing := ", ", ""
	if indexType := lowerer.typeInfo.Types[link.Index].Type; !types.Identical(indexType, types.Typ[types.Int]) && !isUntyped(indexType) {
		open, closing = ", int(", ")"
	}
	condition := fmt.Sprintf("0 <= %s && %s < len(%s)", names.index, names.index, names.value)
	if kind == arrayPointerIndex {
		condition = fmt.Sprintf("%s != nil && %s", names.value, condition)
	}
	lowerer.replace(link.X.End(), link.Lbrack+1, open, line)
	lowerer.replace(link.Rbrack, link.Rbrack+1, fmt.Sprintf("%s; %s {\n%s[%s]", closing, condition, next, names.index), line)
}

func isUntyped(goType types.Type) bool {
	basic, ok := goType.(*types.Basic)
	return ok && basic.Info()&types.IsUntyped != 0
}

// lowerCoalesce lowers a ?? chain to one guard for each left operand:
//
//	func() T { if v := a; v != nil { return *v }; if v := b; ...; return d }()
func (lowerer *lowerer) lowerCoalesce(call *ast.CallExpr) {
	if lowerer.coalesceOperand(call) {
		return // the enclosing ?? lowers it
	}
	allOperands := lowerer.operands(call)
	leftOperands, fallback := allOperands[:len(allOperands)-1], allOperands[len(allOperands)-1]
	if slices.ContainsFunc(append(allOperands, lowerer.parentExpr(call)), isLogicalOperation) {
		lowerer.errorf(lowerer.file, allOperands[0].Pos(), "?? cannot be mixed with || or && without parentheses")
		return
	}
	resultType := lowerer.typeInfo.Types[call].Type
	typeText, ok := lowerer.typeName(resultType)
	if !ok {
		lowerer.typeNameTakenError(allOperands[0].Pos(), "??", typeText)
		return
	}
	if lowerer.reportHiddenType(resultType, typeText, allOperands[0].Pos()) {
		return
	}
	if !lowerer.checkBuiltins(allOperands[0].Pos(), "??", "nil") {
		return
	}
	chain := coalesceChain{
		resultType: resultType,
		typeText:   typeText,
		tempVar:    lowerer.pickTempVar(call.Pos(), fallback.End(), nil),
		depth:      lowerer.depth(call),
	}
	line := lowerer.line(call.Pos())
	for operandIndex, leftOperand := range leftOperands {
		open := ""
		if operandIndex == 0 {
			open = "func() " + typeText + " {\n"
		}
		closing, ok := lowerer.coalesceGuard(chain, leftOperand, open)
		if !ok {
			return
		}
		separator := "\n"
		if operandIndex == len(leftOperands)-1 {
			separator = "\nreturn "
		}
		lowerer.replace(leftOperand.End(), allOperands[operandIndex+1].Pos(), closing+separator, line)
	}
	lowerer.insertOrdered(fallback.End(), "\n}()", line, -chain.depth)
}

func isLogicalOperation(expr ast.Expr) bool {
	binary, ok := expr.(*ast.BinaryExpr)
	return ok && (binary.Op == token.LOR || binary.Op == token.LAND)
}

// coalesceChain holds the data that all operands of one ?? chain share.
type coalesceChain struct {
	resultType types.Type
	typeText   string
	tempVar    string
	depth      int
}

// coalesceGuard emits the guards for one left operand of ?? and returns
// the closing braces.
func (lowerer *lowerer) coalesceGuard(chain coalesceChain, leftOperand ast.Expr, open string) (string, bool) {
	var links []ast.Expr
	if lowerer.optionalRoots[leftOperand] {
		var ok bool
		if links, ok = lowerer.links(leftOperand); !ok {
			return "", false
		}
	}
	operandType := lowerer.typeInfo.Types[leftOperand].Type
	isOperandNilable := isComparableToNil(operandType)
	if !isOperandNilable && len(links) == 0 {
		if _, isTypeParam := operandType.(*types.TypeParam); isTypeParam {
			lowerer.errorf(lowerer.file, leftOperand.Pos(), "left side of ?? cannot be compared with nil: type parameter %s allows types that cannot be nil", displayName(operandType))
		} else {
			lowerer.errorf(lowerer.file, leftOperand.Pos(), "left side of ?? is never nil")
		}
		return "", false
	}
	value, ok := lowerer.coalescedValue(chain, leftOperand, isOperandNilable)
	if !ok {
		return "", false
	}
	tempVar := chain.tempVar
	if !isOperandNilable {
		return lowerer.guards(leftOperand, open, links, tempVar, "return "), true
	}
	closing := lowerer.guards(leftOperand, open, links, tempVar, "if "+tempVar+" := ")
	lowerer.insertOrdered(leftOperand.End(), fmt.Sprintf("; %s != nil {\nreturn %s\n}", tempVar, value), lowerer.line(leftOperand.Pos()), -chain.depth)
	return closing, true
}

// coalescedValue returns the expression for a non-nil left operand. It
// dereferences the operand if ?? unwraps a pointer to the result type.
func (lowerer *lowerer) coalescedValue(chain coalesceChain, leftOperand ast.Expr, isOperandNilable bool) (string, bool) {
	operandType := lowerer.typeInfo.Types[leftOperand].Type
	elemType, isPointer := pointerElem(operandType)
	if isOperandNilable && isPointer && !types.Identical(operandType, chain.resultType) && types.AssignableTo(elemType, chain.resultType) {
		return "*" + chain.tempVar, true
	}
	if !types.AssignableTo(operandType, chain.resultType) {
		lowerer.errorf(lowerer.file, leftOperand.Pos(), "?? operands have incompatible types %s and %s", displayName(operandType), chain.typeText)
		return "", false
	}
	return chain.tempVar, true
}

func (lowerer *lowerer) lowerChain(root ast.Expr) {
	_, parent := outermostParen(lowerer.parents, root)
	if isAddressOf(parent) {
		lowerer.errorf(lowerer.file, root.Pos(), "cannot take the address of an optional chain")
		return
	}
	if isDereference(parent) {
		lowerer.errorf(lowerer.file, root.Pos(), "cannot dereference an optional chain; it gives nil when the chain is skipped")
		return
	}
	if lowerer.isAssignTarget(root) {
		lowerer.errorf(lowerer.file, root.Pos(), "an optional chain cannot be the target of an assignment")
		return
	}
	links, ok := lowerer.links(root)
	if !ok || !lowerer.checkBuiltins(root.Pos(), "optional chain", "nil") {
		return
	}
	check := lowerer.optionalChecks[root]
	tempVar := lowerer.pickTempVar(root.Pos(), root.End(), check.try)
	if lowerer.isStatement(root) {
		lowerer.dropParens(root)
		closing := lowerer.guards(root, "", links, tempVar, check.prefix)
		lowerer.insertOrdered(root.End(), check.suffix+closing, lowerer.line(root.Pos()), -lowerer.depth(root))
		return
	}
	resultType := lowerer.typeInfo.Types[root].Type
	if _, isTuple := resultType.(*types.Tuple); isTuple || resultType == nil {
		lowerer.errorf(lowerer.file, root.Pos(), "an optional call with zero or multiple results can only be used as a statement")
		return
	}
	typeText, ok := lowerer.typeName(resultType)
	if !ok {
		lowerer.typeNameTakenError(root.Pos(), "optional chain", typeText)
		return
	}
	if lowerer.reportHiddenType(resultType, typeText, root.Pos()) {
		return
	}
	zeroText := zeroValue(resultType, typeText)
	if !lowerer.checkBuiltins(root.Pos(), "optional chain", zeroValueBuiltins(zeroText)...) {
		return
	}
	closing := lowerer.guards(root, "func() "+typeText+" {\n", links, tempVar, "return ")
	lowerer.insertOrdered(root.End(), closing+"\nreturn "+zeroText+"\n}()", lowerer.line(root.Pos()), -lowerer.depth(root))
}

func isAddressOf(node ast.Node) bool {
	unaryExpr, ok := node.(*ast.UnaryExpr)
	return ok && unaryExpr.Op == token.AND
}

// isAssignTarget reports whether expr, maybe in parentheses, is assigned to
// or incremented.
func (lowerer *lowerer) isAssignTarget(expr ast.Expr) bool {
	outerExpr, parent := outermostParen(lowerer.parents, expr)
	for _, target := range assignTargets(parent) {
		if target == outerExpr {
			return true
		}
	}
	return false
}

func isDereference(node ast.Node) bool {
	_, ok := node.(*ast.StarExpr)
	return ok
}

// The ?? operator type-checks as one of these calls. With the pointer form,
// `p ?? 0` with p of type *int64 gives int64, not int.
const (
	coalesceMarker    = "__goslingN"
	coalescePtrMarker = "__goslingNP"
)
