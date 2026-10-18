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
// Each piece of source text appears once, in its original order. So only
// the text between pieces changes, and edits inside the pieces survive.

// findChains records the parent of each node and the root of each optional
// chain. The root is the outermost selector, call, or index expression
// that continues the chain.
func (lowerer *lowerer) findChains() {
	lowerer.parents = parents(lowerer.file.ast)
	lowerer.optionalRoots = map[ast.Expr]bool{}
	for selector := range lowerer.file.optional {
		var root ast.Expr = selector
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

// links returns the optional selectors of the chain that ends at root, in
// source order. It reports an error if the left side of a link cannot be nil.
func (lowerer *lowerer) links(root ast.Expr) ([]*ast.SelectorExpr, bool) {
	links := lowerer.collectOptionalLinks(root)
	link := lowerer.findNonNilableLink(links)
	if link == nil {
		return links, true
	}
	leftType := lowerer.typeInfo.Types[link.X].Type
	if _, isTypeParam := leftType.(*types.TypeParam); isTypeParam {
		lowerer.errorf(lowerer.file, link.X.End(), "?. needs a pointer or interface on its left, but type parameter %s allows types that cannot be nil", displayName(leftType))
	} else {
		lowerer.errorf(lowerer.file, link.X.End(), "?. needs a pointer or interface on its left, not %s", displayName(leftType))
	}
	return nil, false
}

// collectOptionalLinks returns the optional selectors of the chain that ends at
// root, in source order.
func (lowerer *lowerer) collectOptionalLinks(root ast.Expr) []*ast.SelectorExpr {
	var links []*ast.SelectorExpr
	for expr := root; expr != nil; expr = spineChild(expr) {
		if selector, ok := expr.(*ast.SelectorExpr); ok && lowerer.file.optional[selector] {
			links = append(links, selector)
		}
	}
	slices.Reverse(links)
	return links
}

// findNonNilableLink returns the first link whose left side cannot be nil, or nil.
func (lowerer *lowerer) findNonNilableLink(links []*ast.SelectorExpr) *ast.SelectorExpr {
	for _, link := range links {
		if !isComparableToNil(lowerer.typeInfo.Types[link.X].Type) {
			return link
		}
	}
	return nil
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
func (lowerer *lowerer) guards(expr ast.Expr, open string, links []*ast.SelectorExpr, tempVar, lastPrefix string) string {
	start, line := expr.Pos(), lowerer.line(expr.Pos())
	base := expr
	if len(links) > 0 {
		base = links[0].X
	}
	if hasBareCompositeLit(base) {
		// A composite literal in an if header needs parentheses.
		lowerer.insertOrdered(start, open+"if "+tempVar+" := (", line, lowerer.depth(expr))
		lowerer.insertOrdered(base.End(), ")", line, -lowerer.depth(expr))
	} else {
		lowerer.insertOrdered(start, open+"if "+tempVar+" := ", line, lowerer.depth(expr))
	}
	for linkIndex, link := range links {
		text := fmt.Sprintf("; %s != nil {\nif %s := %s.", tempVar, tempVar, tempVar)
		if linkIndex == len(links)-1 {
			text = fmt.Sprintf("; %s != nil {\n%s%s.", tempVar, lastPrefix, tempVar)
		}
		lowerer.replace(link.X.End(), link.Sel.Pos(), text, line)
	}
	return strings.Repeat("\n}", len(links))
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
	chain := coalesceChain{
		resultType: resultType,
		typeText:   typeText,
		tempVar:    lowerer.pickTempVar(call.Pos(), fallback.End()),
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
	var links []*ast.SelectorExpr
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
	if _, parent := outermostParen(lowerer.parents, root); isAddressOf(parent) {
		lowerer.errorf(lowerer.file, root.Pos(), "cannot take the address of an optional chain")
		return
	}
	if lowerer.isAssignTarget(root) {
		lowerer.errorf(lowerer.file, root.Pos(), "an optional chain cannot be the target of an assignment")
		return
	}
	links, ok := lowerer.links(root)
	if !ok {
		return
	}
	tempVar := lowerer.pickTempVar(root.Pos(), root.End())
	if stmt, ok := lowerer.parents[root].(*ast.ExprStmt); ok && stmt.X == root {
		closing := lowerer.guards(root, "", links, tempVar, "")
		lowerer.insertOrdered(root.End(), closing, lowerer.line(root.Pos()), -lowerer.depth(root))
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
	closing := lowerer.guards(root, "func() "+typeText+" {\n", links, tempVar, "return ")
	lowerer.insertOrdered(root.End(), closing+"\nreturn "+zeroValue(resultType, typeText)+"\n}()", lowerer.line(root.Pos()), -lowerer.depth(root))
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

// The ?? operator type-checks as one of these calls. With the pointer form,
// `p ?? 0` with p of type *int64 gives int64, not int.
const (
	coalesceMarker    = "__goslingN"
	coalescePtrMarker = "__goslingNP"
)
