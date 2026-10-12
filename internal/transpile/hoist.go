package transpile

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"
)

// A call that returns (T, error) and is used as a value in a try block, as
// in g(f()), moves into its own checked statement:
//
//	val, err := f()
//	if err != nil { ...jump to catch... }
//	g(val)
//
// For type checking, it is wrapped as __goslingV(f()), which gives the T.
const hoistMarker = "__goslingV"

// wrapNested wraps each call in a try block that returns (T, error) and is
// used as a value. It reports whether anything changed.
func (transpiler *transpiler) wrapNested(file *file, info *types.Info) bool {
	parentOf := parents(file.ast)
	regions := tryRegions(file, parentOf)
	if len(regions) == 0 {
		return false
	}
	var calls []*ast.CallExpr
	ast.Inspect(file.ast, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isValueTuple(info, call) || !regions.contain(parentOf, call) || !transpiler.singleValueUse(info, parentOf, call) {
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
func isValueTuple(info *types.Info, call *ast.CallExpr) bool {
	tuple, ok := info.Types[call].Type.(*types.Tuple)
	return ok && tuple.Len() == 2 && types.Identical(tuple.At(1).Type(), errorType)
}

// singleValueUse reports whether call is where Go needs a single value.
// Positions that accept both results do not count. The statement-level
// checks handle them, or they forward both results.
func (transpiler *transpiler) singleValueUse(info *types.Info, parentOf map[ast.Node]ast.Node, call *ast.CallExpr) bool {
	child, parent := outermostParen(parentOf, call)
	switch parentNode := parent.(type) {
	case *ast.ExprStmt:
		return false
	case *ast.AssignStmt:
		return len(parentNode.Rhs) != 1
	case *ast.GoStmt, *ast.DeferStmt:
		return false
	case *ast.ValueSpec:
		return len(parentNode.Values) != 1 || len(parentNode.Names) != 2 // var a, err = f() takes both
	case *ast.ReturnStmt:
		if len(parentNode.Results) != 1 {
			return true
		}
		signature := enclosingSig(info, parentOf, parentNode)
		return signature == nil || signature.Results().Len() != 2
	case *ast.CallExpr:
		if len(parentNode.Args) != 1 || parentNode.Args[0] != child {
			return true
		}
		signature, ok := info.Types[parentNode.Fun].Type.(*types.Signature)
		return !ok || signature.Params().Len() != 2
	}
	return true
}

// hoist moves the wrapped calls in exprs, which belong to stmt, into checked
// statements before stmt. Deeper calls go first, because the text of a call
// includes the temporaries of its nested calls.
func (lowerer *lowerer) hoist(stmt ast.Stmt, try *tryContext, exprs ...ast.Node) {
	markers := lowerer.markersIn(exprs...)
	if len(markers) == 0 {
		return
	}
	anchor := lowerer.withLabels(stmt)
	target := try.target()
	for _, marker := range markers {
		call := lowerer.file.hoisted[marker]
		lowerer.consumedHoists[marker] = true
		if target == nil {
			lowerer.errorf(lowerer.file, call.Pos(), errUncheckableCall)
			continue
		}
		if lowerer.conditional(marker, stmt) {
			lowerer.errorf(lowerer.file, call.Pos(), "this call can fail but only runs conditionally here, so its error cannot be checked; move it into its own statement")
			continue
		}
		valueVar := lowerer.fresh("val")
		errVar := lowerer.pickErrorVar(try, lowerer.scopeAt(anchor.Pos()), anchor.Pos(), token.NoPos)
		target.jumps++
		offset := lowerer.offset(anchor.Pos())
		lowerer.edits = append(lowerer.edits, edit{
			start:  offset,
			end:    offset,
			line:   lowerer.line(call.Pos()),
			order:  -lowerer.depth(marker),
			move:   &[2]int{lowerer.offset(call.Pos()), lowerer.offset(call.End())},
			before: valueVar + ", " + errVar + " := ",
			after:  "\nif " + errVar + " != nil {\n" + lowerer.failTo(try, target, errVar) + "\n}\n",
		})
		lowerer.drop(call.Pos(), call.End(), valueVar, 0)
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

// conditional reports whether marker runs only on some paths through stmt.
// This is on the right of && or ||, as a ?? default, or after a ?. link.
func (lowerer *lowerer) conditional(marker ast.Node, stmt ast.Stmt) bool {
	child := marker
	for ancestor := lowerer.parents[marker]; ancestor != nil && ancestor != stmt; child, ancestor = ancestor, lowerer.parents[ancestor] {
		switch ancestor := ancestor.(type) {
		case *ast.BinaryExpr:
			if (ancestor.Op == token.LAND || ancestor.Op == token.LOR) && ancestor.Y == child {
				return true
			}
		case *ast.CallExpr:
			if lowerer.file.coalesce[ancestor] != nil && len(ancestor.Args) == 2 && ancestor.Args[1] == child {
				return true
			}
		}
		if expr, ok := ancestor.(ast.Expr); ok && lowerer.optionalRoots[expr] {
			if links, _ := lowerer.links(expr); len(links) > 0 && marker.Pos() > links[0].X.End() {
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
