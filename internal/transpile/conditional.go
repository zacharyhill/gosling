package transpile

import (
	"go/ast"
	"strings"
)

// loopConditionPriority puts the hoisted loop condition before the hoisted
// code of the first statement in the loop body. Both go at the same offset.
const loopConditionPriority = -1 << 20

// firstInsertion and lastInsertion sort an insertion before or after all
// other insertions at its offset.
const (
	firstInsertion = -1 << 30
	lastInsertion  = 1 << 30
)

// hoistLoopCondition moves a loop condition with checked calls into the
// loop body. Then the calls run before each iteration:
//
//	for {
//		val, err := f()
//		if err != nil { ...jump to catch... }
//		if !(val > 0) {
//			break
//		}
//		...
//	}
func (lowerer *lowerer) hoistLoopCondition(stmt *ast.ForStmt, try *tryContext) {
	if stmt.Cond == nil {
		return
	}
	site := &hoistSite{
		offset:   lowerer.offset(stmt.Body.Lbrace + 1),
		bound:    stmt,
		from:     stmt.Cond.Pos(),
		end:      stmt.Body.Rbrace,
		priority: loopConditionPriority,
	}
	if !lowerer.hoistInto(site, try, stmt.Cond) {
		return
	}
	// The semicolon ends the piece without a blank line before the body.
	exitPiece := lowerer.operandPiece("if !(", stmt.Cond, ") {\nbreak\n};", lowerer.line(stmt.Cond.Pos()))
	if exitPiece.moved == nil {
		exitPiece.before = strings.Replace(exitPiece.before, "!(", "!", 1)
		exitPiece.before = strings.Replace(exitPiece.before, ") {", " {", 1)
	}
	site.add(exitPiece)
	lowerer.flushSite(site)
	lowerer.removeExpr(stmt.Cond)
}

// hoistElseIf puts an else-if with checked calls in its header into a
// block. The calls go in the block, before the if statement:
//
//	} else {
//		val, err := f()
//		if err != nil { ...jump to catch... }
//		if val > 0 {
//			...
//		}
//	}
func (lowerer *lowerer) hoistElseIf(stmt *ast.IfStmt, try *tryContext) {
	site := &hoistSite{offset: lowerer.offset(stmt.Pos()), bound: stmt, from: stmt.Pos(), end: stmt.End()}
	site.add(hoistPiece{before: "{\n", line: lowerer.line(stmt.Pos())})
	if !lowerer.hoistInto(site, try, stmt.Init, stmt.Cond) {
		return
	}
	lowerer.flushSite(site)
	lowerer.insert(stmt.End(), "\n}", lowerer.line(stmt.End()))
}

// hoistSwitch hoists the checked calls in the header and the case
// expressions of a switch. A case expression runs only if no case before it
// matches. So a case with checked calls splits the switch. A default case
// gets the checked calls and a nested switch with the remaining cases:
//
//	switch tag := x; tag {
//	case a:
//		...
//	default:
//		val, err := f()
//		if err != nil { ...jump to catch... }
//		switch tag {
//		case val:
//			...
//		}
//	}
//
// The tag goes into a variable, so it runs only one time.
func (lowerer *lowerer) hoistSwitch(stmt *ast.SwitchStmt, try *tryContext) {
	header := []ast.Node{stmt.Init, stmt.Tag}
	var splits []*ast.CaseClause
	for clauseIndex, clause := range stmt.Body.List {
		caseClause := clause.(*ast.CaseClause)
		if len(lowerer.markersIn(exprNodes(caseClause.List)...)) == 0 {
			continue
		}
		// Later expressions of a case run only if the earlier ones do not match.
		lowerer.rejectMarkers(exprNodes(caseClause.List[1:])...)
		switch {
		case clauseIndex == 0 && stmt.Init == nil && stmt.Tag == nil:
			header = append(header, caseClause.List[0]) // it always runs, and first
		case lowerer.canSplit(stmt, clauseIndex):
			splits = append(splits, caseClause)
		}
	}
	lowerer.hoist(stmt, try, header...)
	if len(splits) > 0 {
		lowerer.splitSwitch(stmt, splits, try)
	}
}

func (lowerer *lowerer) splitSwitch(stmt *ast.SwitchStmt, splits []*ast.CaseClause, try *tryContext) {
	nested, tagVar := "switch {\n", ""
	if stmt.Tag != nil {
		tagVar = lowerer.fresh("tag")
		nested = "switch " + tagVar + " {\n"
	}
	nestingLevels := len(splits)
	var tagPieces []hoistPiece
	switch {
	case stmt.Tag != nil && stmt.Init == nil:
		// The moved call of a hoisted tag would take insertions at its edges.
		if dropIndex, ok := lowerer.hoistDrops[stmt.Tag]; ok {
			lowerer.drops[dropIndex].text = tagVar + " := " + lowerer.drops[dropIndex].text + "; " + tagVar
			break
		}
		line := lowerer.line(stmt.Tag.Pos())
		lowerer.insertOrdered(stmt.Tag.Pos(), tagVar+" := ", line, firstInsertion)
		lowerer.insertOrdered(stmt.Tag.End(), "; "+tagVar, line, lastInsertion)
	case stmt.Tag != nil:
		// The header has no place for the tag variable. A default case
		// around all cases declares it.
		tagPieces = append(tagPieces, lowerer.operandPiece(tagVar+" := ", stmt.Tag, "\n", lowerer.line(stmt.Tag.Pos())))
		if firstClause := stmt.Body.List[0].(*ast.CaseClause); splits[0] != firstClause {
			lowerer.splitCase(stmt, firstClause, nil, tagPieces, nested, try)
			tagPieces = nil
			nestingLevels++
		}
		lowerer.removeExpr(stmt.Tag)
	}
	for _, clause := range splits {
		lowerer.splitCase(stmt, clause, clause.List[0], tagPieces, nested, try)
		tagPieces = nil
	}
	lowerer.insert(stmt.Body.Rbrace, strings.Repeat("}\n", nestingLevels), lowerer.line(stmt.Body.Rbrace))
}

// splitCase starts a default case and a nested switch before clause. The
// default case holds the checked calls in expr.
func (lowerer *lowerer) splitCase(stmt *ast.SwitchStmt, clause *ast.CaseClause, expr ast.Expr, prefix []hoistPiece, nested string, try *tryContext) {
	line := lowerer.line(clause.Case)
	site := &hoistSite{offset: lowerer.offset(clause.Case), bound: stmt, from: clause.Case, end: stmt.Body.Rbrace}
	site.add(hoistPiece{before: "default:\n", line: line})
	site.pieces = append(site.pieces, prefix...)
	if expr != nil {
		lowerer.hoistInto(site, try, expr)
	}
	site.add(hoistPiece{before: nested, line: line})
	lowerer.flushSite(site)
}

// canSplit reports whether the switch can split before the clause at
// clauseIndex. It reports an error if not.
func (lowerer *lowerer) canSplit(stmt *ast.SwitchStmt, clauseIndex int) bool {
	clause := stmt.Body.List[clauseIndex].(*ast.CaseClause)
	message := ""
	for _, earlier := range stmt.Body.List[:clauseIndex] {
		if earlier.(*ast.CaseClause).List == nil {
			message = "a case with a call that can fail must come before the default case"
		}
	}
	if clauseIndex > 0 && endsInFallthrough(stmt.Body.List[clauseIndex-1].(*ast.CaseClause).Body) {
		message = "fallthrough into a case with a call that can fail is not supported"
	}
	if message == "" {
		return true
	}
	lowerer.errorf(lowerer.file, clause.Case, "%s", message)
	for _, marker := range lowerer.markersIn(exprNodes(clause.List)...) {
		lowerer.consumedHoists[marker] = true
	}
	return false
}

// rejectMarkers reports the wrapped calls in nodes. They run only on some
// paths, and gosling cannot rewrite these paths.
func (lowerer *lowerer) rejectMarkers(nodes ...ast.Node) {
	for _, marker := range lowerer.markersIn(nodes...) {
		lowerer.consumedHoists[marker] = true
		lowerer.errorf(lowerer.file, lowerer.file.hoisted[marker].Pos(), errConditionalCall)
	}
}

func exprNodes(exprs []ast.Expr) []ast.Node {
	nodes := make([]ast.Node, len(exprs))
	for exprIndex, expr := range exprs {
		nodes[exprIndex] = expr
	}
	return nodes
}
