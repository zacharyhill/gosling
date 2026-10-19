package transpile

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"
)

// A try block can use a call that returns (T, error) as a value, as in
// g(f()). The call then moves into its own checked statement:
//
//	val, err := f()
//	if err != nil { ...jump to catch... }
//	g(val)
//
// For type checking, gosling wraps the call as __goslingV(f()), which gives the T.
const hoistMarker = "__goslingV"

// wrapNested wraps each call in a try block that returns (T, error) and is
// used as a value. It reports whether anything changed.
func (transpiler *transpiler) wrapNested(file *file, typeInfo *types.Info) bool {
	parentOf := parents(file.ast)
	regions := tryRegions(file, parentOf)
	if len(regions) == 0 {
		return false
	}
	var calls []*ast.CallExpr
	ast.Inspect(file.ast, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isValueTuple(typeInfo, call) || !regions.contain(parentOf, call) || !transpiler.singleValueUse(typeInfo, parentOf, call) {
			return true
		}
		if parentCall, ok := parentOf[call].(*ast.CallExpr); ok && file.hoisted[parentCall] != nil {
			return true // already wrapped
		}
		calls = append(calls, call)
		return true
	})
	for _, call := range calls {
		marker := &ast.CallExpr{Fun: &ast.Ident{Name: hoistMarker, NamePos: call.Pos()}, Args: []ast.Expr{call}, Rparen: call.End() - 1}
		replaceNode(file.ast, call, marker)
		file.hoisted[marker] = call
	}
	return len(calls) > 0
}

// tryRegion is the body of one try block and the function holding it.
type tryRegion struct {
	lbrace, rbrace token.Pos
	function       ast.Node
}

type tryRegionList []tryRegion

func tryRegions(file *file, parentOf map[ast.Node]ast.Node) tryRegionList {
	var regions tryRegionList
	ast.Inspect(file.ast, func(node ast.Node) bool {
		if ifStmt, ok := node.(*ast.IfStmt); ok {
			if _, isTry := file.tries[file.tokenFile.Offset(ifStmt.Pos())]; isTry {
				regions = append(regions, tryRegion{ifStmt.Body.Lbrace, ifStmt.Body.Rbrace, enclosingFunc(parentOf, ifStmt)})
			}
		}
		return true
	})
	return regions
}

// contain reports whether call is inside a try body of its own function,
// not inside a nested function literal.
func (regions tryRegionList) contain(parentOf map[ast.Node]ast.Node, call *ast.CallExpr) bool {
	enclosing := enclosingFunc(parentOf, call)
	for _, region := range regions {
		if call.Pos() > region.lbrace && call.End() <= region.rbrace && enclosing == region.function {
			return true
		}
	}
	return false
}

// isValueTuple reports whether the results of call are exactly (T, error).
func isValueTuple(typeInfo *types.Info, call *ast.CallExpr) bool {
	tuple, ok := typeInfo.Types[call].Type.(*types.Tuple)
	return ok && tuple.Len() == 2 && types.Identical(tuple.At(1).Type(), errorType)
}

// singleValueUse reports whether call is where Go needs a single value.
// Positions that accept both results do not count. The statement-level
// checks handle them, or they forward both results.
func (transpiler *transpiler) singleValueUse(typeInfo *types.Info, parentOf map[ast.Node]ast.Node, call *ast.CallExpr) bool {
	child, parent := outermostParen(parentOf, call)
	switch parentNode := parent.(type) {
	case *ast.ExprStmt:
		return false
	case *ast.AssignStmt:
		isPlainAssign := parentNode.Tok == token.ASSIGN || parentNode.Tok == token.DEFINE
		return len(parentNode.Rhs) != 1 || !isPlainAssign // x += f() needs one value
	case *ast.GoStmt, *ast.DeferStmt:
		return false
	case *ast.ValueSpec:
		return len(parentNode.Values) != 1 || len(parentNode.Names) != 2 // var a, err = f() takes both
	case *ast.ReturnStmt:
		if len(parentNode.Results) != 1 {
			return true
		}
		signature := enclosingSignature(typeInfo, parentOf, parentNode)
		return signature == nil || signature.Results().Len() != 2
	case *ast.CallExpr:
		if len(parentNode.Args) != 1 || parentNode.Args[0] != child {
			return true
		}
		signature, ok := typeInfo.Types[parentNode.Fun].Type.(*types.Signature)
		return !ok || signature.Params().Len() != 2
	}
	return true
}

// hoist moves the wrapped calls in exprs, which belong to stmt, into checked
// statements before stmt.
func (lowerer *lowerer) hoist(stmt ast.Stmt, try *tryContext, exprs ...ast.Node) {
	anchor := lowerer.withLabels(stmt)
	site := &hoistSite{
		offset: lowerer.offset(anchor.Pos()),
		bound:  stmt,
		scope:  lowerer.scopeAt(anchor.Pos()),
		from:   anchor.Pos(),
		end:    stmt.End(),
	}
	if lowerer.hoistInto(site, try, exprs...) {
		lowerer.flushSite(site)
	}
}

// hoistSite is a place for hoisted code: before a statement, or at the
// start of a block that gosling adds.
type hoistSite struct {
	offset int
	// bound is the node whose expressions the site hoists.
	bound ast.Node
	// New error variables must not clash with names in [from, end). With a
	// scope, the top level of the site uses the rules of pickErrorVar
	// instead. Then it can reuse an error variable of the scope.
	scope    *types.Scope
	from     token.Pos
	end      token.Pos
	priority int // sorts the site before other insertions at its offset
	pieces   []hoistPiece
}

// hoistPiece is hoisted code: text, or text around a moved expression.
type hoistPiece struct {
	before string
	moved  ast.Expr // nil for plain text
	after  string
	line   int
}

func (site *hoistSite) add(piece hoistPiece) { site.pieces = append(site.pieces, piece) }

// hoistScope holds hoisted items that run together: all items of a site,
// or the items in the right side of a && or || operation. An item is a
// hoist marker or a && or || operation whose right side holds markers.
type hoistScope struct {
	items    []ast.Expr
	children map[*ast.BinaryExpr]*hoistScope
}

func newHoistScope() *hoistScope {
	return &hoistScope{children: map[*ast.BinaryExpr]*hoistScope{}}
}

// hoistInto adds checked statements for the wrapped calls in exprs to site.
// It reports whether it added any.
func (lowerer *lowerer) hoistInto(site *hoistSite, try *tryContext, exprs ...ast.Node) bool {
	target := try.target()
	root := newHoistScope()
	for _, marker := range lowerer.markersIn(exprs...) {
		call := lowerer.file.hoisted[marker]
		lowerer.consumedHoists[marker] = true
		switch {
		case target == nil:
			lowerer.errorf(lowerer.file, call.Pos(), errUncheckableCall)
		case lowerer.isConditional(marker, site.bound):
			lowerer.errorf(lowerer.file, call.Pos(), errConditionalCall)
		default:
			lowerer.addToScope(root, marker, site.bound)
		}
	}
	if len(root.items) == 0 {
		return false
	}
	lowerer.hoistScopeItems(site, root, try, false)
	return true
}

// addToScope adds marker to the scope where it runs. The right side of
// each && or || that holds marker is a nested scope.
func (lowerer *lowerer) addToScope(root *hoistScope, marker *ast.CallExpr, bound ast.Node) {
	var splits []*ast.BinaryExpr // innermost first
	var child ast.Node = marker
	for ancestor := lowerer.parents[marker]; ancestor != nil && ancestor != bound; child, ancestor = ancestor, lowerer.parents[ancestor] {
		if binary, ok := ancestor.(*ast.BinaryExpr); ok && (binary.Op == token.LAND || binary.Op == token.LOR) && binary.Y == child {
			splits = append(splits, binary)
		}
	}
	scope := root
	for splitIndex := len(splits) - 1; splitIndex >= 0; splitIndex-- {
		split := splits[splitIndex]
		nested, ok := scope.children[split]
		if !ok {
			nested = newHoistScope()
			scope.children[split] = nested
			scope.items = append(scope.items, split)
		}
		scope = nested
	}
	scope.items = append(scope.items, marker)
}

// hoistScopeItems adds the items of scope to site in evaluation order. An item
// runs after the items inside it, and after the items to its left.
func (lowerer *lowerer) hoistScopeItems(site *hoistSite, scope *hoistScope, try *tryContext, isNested bool) {
	slices.SortStableFunc(scope.items, func(left, right ast.Expr) int {
		if left.End() != right.End() {
			return int(left.End()) - int(right.End())
		}
		return lowerer.depth(right) - lowerer.depth(left)
	})
	for _, item := range scope.items {
		switch item := item.(type) {
		case *ast.CallExpr:
			lowerer.hoistCall(site, item, try, isNested)
		case *ast.BinaryExpr:
			lowerer.hoistSplit(site, item, scope.children[item], try)
		}
	}
}

// hoistCall moves the call that marker wraps into a checked statement.
func (lowerer *lowerer) hoistCall(site *hoistSite, marker *ast.CallExpr, try *tryContext, isNested bool) {
	call := lowerer.file.hoisted[marker]
	target := try.target()
	valueVar := lowerer.fresh("val")
	scope, end := site.scope, token.NoPos
	if isNested || scope == nil {
		scope, end = nil, site.end
	}
	errVar := lowerer.pickErrorVar(try, scope, site.from, end)
	target.jumps++
	site.add(hoistPiece{
		before: valueVar + ", " + errVar + " := ",
		moved:  call,
		after:  "\nif " + errVar + " != nil {\n" + lowerer.failTo(try, target, errVar) + "\n}\n",
		line:   lowerer.line(call.Pos()),
	})
	lowerer.replaceHoisted(marker, valueVar)
}

// hoistSplit evaluates a && or || operation with checked calls on its
// right side into a variable. The right side runs in an if statement, so
// it keeps short-circuit evaluation:
//
//	cond := ok
//	if cond {
//		val, err := f()
//		if err != nil { ...jump to catch... }
//		cond = val > 0
//	}
func (lowerer *lowerer) hoistSplit(site *hoistSite, split *ast.BinaryExpr, scope *hoistScope, try *tryContext) {
	condVar := lowerer.fresh("cond")
	condTest := condVar
	if split.Op == token.LOR {
		condTest = "!" + condVar
	}
	line := lowerer.line(split.Pos())
	site.add(lowerer.operandPiece(lowerer.condDecl(split, condVar), split.X, "\nif "+condTest+" {\n", line))
	lowerer.hoistScopeItems(site, scope, try, true)
	site.add(lowerer.operandPiece(condVar+" = ", split.Y, "\n}\n", line))
	lowerer.replaceHoisted(split, condVar)
}

// condDecl returns the start of the declaration of the variable for split.
// An untyped left side gives the variable the type bool, so a named boolean
// type needs an explicit type.
func (lowerer *lowerer) condDecl(split *ast.BinaryExpr, condVar string) string {
	splitType := lowerer.typeInfo.Types[split].Type
	if basic, ok := splitType.(*types.Basic); ok && basic.Info()&types.IsBoolean != 0 {
		return condVar + " := "
	}
	if typeText, ok := lowerer.typeName(splitType); ok {
		return "var " + condVar + " " + typeText + " = "
	}
	return condVar + " := "
}

// replaceHoisted replaces expr in the output with the variable that holds
// its value.
func (lowerer *lowerer) replaceHoisted(expr ast.Expr, variable string) {
	lowerer.hoistDrops[expr] = len(lowerer.drops)
	lowerer.drop(expr.Pos(), expr.End(), variable, 0)
}

// operandPiece returns a piece with expr between before and after. A move
// does not apply a drop that covers exactly the moved range, so a hoisted
// expr gives its variable directly.
func (lowerer *lowerer) operandPiece(before string, expr ast.Expr, after string, line int) hoistPiece {
	if dropIndex, ok := lowerer.hoistDrops[expr]; ok {
		return hoistPiece{before: before + lowerer.drops[dropIndex].text + after, line: line}
	}
	return hoistPiece{before: before, moved: expr, after: after, line: line}
}

// removeExpr removes expr from its place in the output. A piece holds its
// code.
func (lowerer *lowerer) removeExpr(expr ast.Expr) {
	if dropIndex, ok := lowerer.hoistDrops[expr]; ok {
		lowerer.drops[dropIndex].text = ""
		return
	}
	lowerer.drop(expr.Pos(), expr.End(), "", 0)
}

// flushSite records the pieces of site as insertions at its offset.
func (lowerer *lowerer) flushSite(site *hoistSite) {
	for pieceIndex, piece := range site.pieces {
		insertion := edit{
			start: site.offset,
			end:   site.offset,
			line:  piece.line,
			order: site.priority - len(site.pieces) + pieceIndex,
		}
		if piece.moved == nil {
			insertion.text = piece.before + piece.after
		} else {
			insertion.move = &[2]int{lowerer.offset(piece.moved.Pos()), lowerer.offset(piece.moved.End())}
			insertion.before, insertion.after = piece.before, piece.after
		}
		lowerer.edits = append(lowerer.edits, insertion)
	}
}

// withLabels returns the outermost labeled statement that wraps stmt, or
// stmt. So hoisted code goes before the labels, not between a label and its loop.
func (lowerer *lowerer) withLabels(stmt ast.Stmt) ast.Node {
	var anchor ast.Node = stmt
	for {
		labeled, ok := lowerer.parents[anchor].(*ast.LabeledStmt)
		if !ok {
			return anchor
		}
		anchor = labeled
	}
}

// markersIn returns the hoist markers in nodes, outside function literals,
// deepest first.
func (lowerer *lowerer) markersIn(nodes ...ast.Node) []*ast.CallExpr {
	var markers []*ast.CallExpr
	for _, root := range nodes {
		if root == nil {
			continue
		}
		ast.Inspect(root, func(node ast.Node) bool {
			if _, ok := node.(*ast.FuncLit); ok {
				return false
			}
			if call, ok := node.(*ast.CallExpr); ok && lowerer.file.hoisted[call] != nil {
				markers = append(markers, call)
			}
			return true
		})
	}
	slices.Reverse(markers)
	return markers
}

// isConditional reports whether marker runs only on some paths through bound
// that gosling cannot rewrite: as a ?? default, or after a ?. link.
func (lowerer *lowerer) isConditional(marker ast.Node, bound ast.Node) bool {
	child := marker
	for ancestor := lowerer.parents[marker]; ancestor != nil && ancestor != bound; child, ancestor = ancestor, lowerer.parents[ancestor] {
		if call, ok := ancestor.(*ast.CallExpr); ok && lowerer.file.coalesce[call] != nil && len(call.Args) == 2 && call.Args[1] == child {
			return true
		}
		if expr, ok := ancestor.(ast.Expr); ok && lowerer.optionalRoots[expr] {
			// The chain lowering reports an invalid chain, so skip it here.
			links := lowerer.collectOptionalLinks(expr)
			if len(links) > 0 && lowerer.findNonNilableLink(links) == nil && marker.Pos() > links[0].X.End() {
				return true
			}
		}
	}
	return false
}

// failTo returns the code that sends errVar to target's catch block from a
// failure inside try.
func (lowerer *lowerer) failTo(try, target *tryContext, errVar string) string {
	jumpCode := lowerer.jumpOut(try, target, "goto "+target.catchLabel())
	if target.catchVar != "" && errVar != target.catchVar {
		return target.catchVar + " = " + errVar + "\n" + jumpCode
	}
	return jumpCode
}
