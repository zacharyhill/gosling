package transpile

import (
	"bytes"
	"go/ast"
	"go/token"
	"go/types"
	"strings"
)

type lowerer struct {
	*transpiler
	file           *file
	typeInfo       *types.Info
	edits          []edit
	identPositions map[string][]token.Pos
	lastTryID      int
	// drops replace whole regions, such as an unreachable catch block.
	// They discard the edits inside them.
	drops          []edit
	parents        map[ast.Node]ast.Node
	consumedHoists map[*ast.CallExpr]bool
	// checkedAssigns holds the augmented assignments that lowering checked or reported.
	checkedAssigns map[*ast.AssignStmt]bool
	optionalRoots  map[ast.Expr]bool
	// hoistDrops maps each hoisted expression to the drop that replaces it
	// with its variable.
	hoistDrops   map[ast.Expr]int
	namedResults map[types.Object]bool
	// wrappedHeaders holds the statements whose init statement moved into a new block.
	wrappedHeaders map[ast.Stmt]bool
	// optionalChecks holds the error checks for optional calls in try
	// blocks. The check goes inside the nil guards of the chain.
	optionalChecks map[ast.Expr]optionalCheck
}

func newLowerer(transpiler *transpiler, file *file, typeInfo *types.Info) *lowerer {
	lowerer := &lowerer{
		transpiler:     transpiler,
		file:           file,
		typeInfo:       typeInfo,
		identPositions: map[string][]token.Pos{},
		consumedHoists: map[*ast.CallExpr]bool{},
		hoistDrops:     map[ast.Expr]int{},
		wrappedHeaders: map[ast.Stmt]bool{},
		checkedAssigns: map[*ast.AssignStmt]bool{},
		namedResults:   map[types.Object]bool{},
		optionalChecks: map[ast.Expr]optionalCheck{},
	}
	ast.Inspect(file.ast, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.Ident:
			if node.Pos().IsValid() {
				lowerer.identPositions[node.Name] = append(lowerer.identPositions[node.Name], node.Pos())
			}
		case *ast.FuncType:
			lowerer.addNamedResults(node)
		}
		return true
	})
	lowerer.findChains()
	return lowerer
}

func (lowerer *lowerer) addNamedResults(funcType *ast.FuncType) {
	if funcType.Results == nil {
		return
	}
	for _, field := range funcType.Results.List {
		for _, resultIdent := range field.Names {
			if object := lowerer.typeInfo.Defs[resultIdent]; object != nil {
				lowerer.namedResults[object] = true
			}
		}
	}
}

func (lowerer *lowerer) lowerFile() {
	for _, decl := range lowerer.file.ast.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok {
			lowerer.walkExprs(decl)
			continue
		}
		if funcDecl.Body == nil {
			continue
		}
		lowerer.walkBlock(funcDecl.Body.List, nil)
	}
	lowerer.checkLiteralConstants()
	lowerer.lowerConsts()
	lowerer.lowerTryExpressions()
	lowerer.lowerTuples()
	lowerer.lowerTypedHeaders()
	lowerer.lowerTemplates()
	lowerer.lowerNullish()
	lowerer.lowerDefaults()
	lowerer.lowerTernaries()
	lowerer.lowerArrows()
	lowerer.lowerArrays()
	lowerer.lowerLengths()
	lowerer.lowerStrings()
	lowerer.lowerUnions()
	lowerer.lowerEnums()
	lowerer.lowerSpread()
	lowerer.lowerForOfBlanks()
	lowerer.lowerOptionalValues()
	lowerer.emitImports()
	for body := range lowerer.file.panicAtEnd {
		lowerer.checkBuiltins(body.Rbrace, "try", "panic")
		lowerer.insert(body.Rbrace, "panic(\"unreachable\")\n", lowerer.line(body.Rbrace))
	}
	for _, splitOffset := range lowerer.file.preprocessed.Splits {
		lowerer.replaceOffsets(splitOffset, splitOffset+len("; "), "\n", lowerer.line(lowerer.file.tokenFile.Pos(splitOffset)))
	}
	// When gosling removes the added newlines, the next token gets a line directive.
	for _, newlineRange := range lowerer.file.preprocessed.Newlines {
		lowerer.replaceOffsets(newlineRange[0], newlineRange[1], "", 0)
	}
	for tryOffset := range lowerer.file.tries {
		lowerer.errorf(lowerer.file, lowerer.file.tokenFile.Pos(tryOffset), "try is only supported as a statement")
	}
	for marker, call := range lowerer.file.hoisted {
		if !lowerer.consumedHoists[marker] {
			lowerer.errorf(lowerer.file, call.Pos(), errUncheckableCall)
		}
	}
	lowerer.reportUncheckedAssigns()
}

func (lowerer *lowerer) walkBlock(list []ast.Stmt, try *tryContext) {
	for _, stmt := range list {
		lowerer.walkStmt(stmt, try)
	}
}

// walkExprs lowers function literals nested in nodes. Each literal starts a
// new function context, because goto and return cannot cross it.
func (lowerer *lowerer) walkExprs(nodes ...ast.Node) {
	for _, root := range nodes {
		if root == nil {
			continue
		}
		ast.Inspect(root, func(node ast.Node) bool {
			funcLit, ok := node.(*ast.FuncLit)
			if !ok {
				return true
			}
			lowerer.walkBlock(funcLit.Body.List, nil)
			return false
		})
	}
}

func (lowerer *lowerer) walkStmt(stmt ast.Stmt, try *tryContext) {
	switch stmt := stmt.(type) {
	case *ast.IfStmt:
		lowerer.walkIf(stmt, try)
	case *ast.BlockStmt:
		if !lowerer.file.finallyBlocks[stmt] { // lowered with its try statement
			lowerer.walkBlock(stmt.List, try)
		}
	case *ast.LabeledStmt:
		lowerer.walkStmt(stmt.Stmt, try)
	case *ast.ForStmt:
		lowerer.hoist(stmt, try, stmt.Init)
		lowerer.hoistLoopCondition(stmt, try)
		lowerer.lowerInit(stmt, try)
		lowerer.walkExprs(stmt.Init, stmt.Cond, stmt.Post)
		lowerer.walkBlock(stmt.Body.List, try)
	case *ast.RangeStmt:
		lowerer.hoist(stmt, try, stmt.X)
		lowerer.walkExprs(stmt.X)
		lowerer.walkBlock(stmt.Body.List, try)
	case *ast.SwitchStmt:
		lowerer.checkLiteralSwitch(stmt)
		lowerer.checkEnumSwitch(stmt)
		lowerer.hoistSwitch(stmt, try)
		lowerer.lowerInit(stmt, try)
		lowerer.walkExprs(stmt.Init, stmt.Tag)
		lowerer.walkClauses(stmt.Body, try)
	case *ast.TypeSwitchStmt:
		lowerer.checkExhaustive(stmt)
		if !lowerer.hoistAfterInit(stmt, try, stmt.Init, "switch ", stmt.Assign) {
			lowerer.hoist(stmt, try, stmt.Init, stmt.Assign)
		}
		lowerer.lowerInit(stmt, try)
		lowerer.walkExprs(stmt.Init, stmt.Assign)
		lowerer.walkClauses(stmt.Body, try)
	case *ast.SelectStmt:
		lowerer.walkClauses(stmt.Body, try)
	default:
		lowerer.hoist(stmt, try, stmt)
		if !lowerer.lowerThrow(stmt, try) && !lowerer.lowerExit(stmt, try) {
			lowerer.lowerTryStmt(stmt, try)
		}
		lowerer.walkExprs(stmt)
	}
}

// walkIf lowers an if statement, or the try statement that the if stands for.
func (lowerer *lowerer) walkIf(stmt *ast.IfStmt, try *tryContext) {
	if preprocessedTry, ok := lowerer.file.tries[lowerer.offset(stmt.Pos())]; ok {
		delete(lowerer.file.tries, lowerer.offset(stmt.Pos()))
		lowerer.lowerTry(stmt, preprocessedTry, try)
		return
	}
	if parent, ok := lowerer.parents[stmt].(*ast.IfStmt); ok && parent.Else == stmt {
		lowerer.hoistElseIf(stmt, try)
	} else if !lowerer.hoistAfterInit(stmt, try, stmt.Init, "if ", stmt.Cond) {
		lowerer.hoist(stmt, try, stmt.Init, stmt.Cond)
	}
	lowerer.lowerInit(stmt, try)
	lowerer.walkExprs(stmt.Init, stmt.Cond)
	lowerer.walkBlock(stmt.Body.List, try)
	if stmt.Else != nil {
		lowerer.walkStmt(stmt.Else, try)
	}
}

// lowerInit checks a call in the init statement of an if, switch, or for. A new block holds the
// init, the check, and the statement. The labels move into the block, so break and continue
// still work. In an else-if, the block becomes the else branch, so the call runs only on that path.
func (lowerer *lowerer) lowerInit(stmt ast.Stmt, try *tryContext) {
	init, newHeader, keptHeaderStart := lowerer.splitHeader(stmt)
	if init == nil || !lowerer.lowerTryStmt(init, try) || lowerer.wrappedHeaders[stmt] {
		return
	}
	anchor := lowerer.withLabels(stmt)
	line := lowerer.line(stmt.Pos())
	lowerer.replace(anchor.Pos(), init.Pos(), "{\n", line)
	lowerer.replace(init.End(), keptHeaderStart, "\n"+lowerer.buildLabelPrefix(stmt)+newHeader, line)
	lowerer.insertOrdered(stmt.End(), "\n}", lowerer.line(stmt.End()), -lowerer.depth(stmt))
}

// hoistAfterInit hoists the calls in the header of an if or switch statement
// when the condition has checked calls. Those calls can use the init variables,
// so the init statement and the calls move into a new block:
// { x := g(); val, err := f(x); ...; if x > val { ... } }
// It reports false if the condition has no checked calls or no init statement.
func (lowerer *lowerer) hoistAfterInit(stmt ast.Stmt, try *tryContext, init ast.Stmt, keyword string, condition ast.Node) bool {
	if init == nil || condition == nil || len(lowerer.markersIn(condition)) == 0 {
		return false
	}
	lowerer.hoist(stmt, try, init)
	line := lowerer.line(stmt.Pos())
	// The error check of a checked init statement has order 0 and goes first.
	lowerer.insertOrdered(init.End(), "\n", line, 1)
	lowerer.hoistAt(stmt, init.End(), lastInsertion, try, condition)
	anchor := lowerer.withLabels(stmt)
	lowerer.replace(anchor.Pos(), init.Pos(), "{\n", line)
	lowerer.replace(init.End(), condition.Pos(), lowerer.buildLabelPrefix(stmt)+keyword, line)
	lowerer.insertOrdered(stmt.End(), "\n}", lowerer.line(stmt.End()), -lowerer.depth(stmt))
	lowerer.wrappedHeaders[stmt] = true
	return true
}

func (lowerer *lowerer) buildLabelPrefix(stmt ast.Stmt) string {
	labels := ""
	for node := lowerer.parents[stmt]; node != nil; node = lowerer.parents[node] {
		labeled, ok := node.(*ast.LabeledStmt)
		if !ok {
			break
		}
		labels = labeled.Label.Name + ":\n" + labels
	}
	return labels
}

// splitHeader returns the init statement of stmt, the header text that
// replaces the init statement, and the position where the kept header starts.
func (lowerer *lowerer) splitHeader(stmt ast.Stmt) (init ast.Stmt, newHeader string, keptHeaderStart token.Pos) {
	switch stmt := stmt.(type) {
	case *ast.IfStmt:
		return stmt.Init, "if ", stmt.Cond.Pos()
	case *ast.ForStmt:
		if stmt.Init == nil {
			return nil, "", token.NoPos
		}
		return stmt.Init, "for " + lowerer.buildLoopVarCopies(stmt.Init), stmt.Init.End()
	case *ast.SwitchStmt:
		if stmt.Tag == nil {
			return stmt.Init, "switch ", stmt.Body.Lbrace
		}
		return stmt.Init, "switch ", stmt.Tag.Pos()
	case *ast.TypeSwitchStmt:
		return stmt.Init, "switch ", stmt.Assign.Pos()
	}
	return nil, "", token.NoPos
}

// buildLoopVarCopies returns `i := i` for each variable that init declares. So each
// iteration still gets its own copy, as Go gives to variables of the loop header.
func (lowerer *lowerer) buildLoopVarCopies(init ast.Stmt) string {
	assign, ok := init.(*ast.AssignStmt)
	if !ok || assign.Tok != token.DEFINE {
		return ""
	}
	var names []string
	for _, leftSide := range assign.Lhs {
		if ident, ok := leftSide.(*ast.Ident); ok && ident.Name != "_" && lowerer.typeInfo.Defs[ident] != nil {
			names = append(names, ident.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, ", ") + " := " + strings.Join(names, ", ")
}

func (lowerer *lowerer) walkClauses(body *ast.BlockStmt, try *tryContext) {
	for _, clause := range body.List {
		switch clause := clause.(type) {
		case *ast.CaseClause:
			for _, expr := range clause.List {
				lowerer.walkExprs(expr)
			}
			lowerer.walkBlock(clause.Body, try)
		case *ast.CommClause:
			lowerer.walkExprs(clause.Comm)
			lowerer.walkBlock(clause.Body, try)
		}
	}
}

func (lowerer *lowerer) insert(position token.Pos, text string, line int) {
	lowerer.insertOrdered(position, text, line, 0)
}

// insertAfter adds inline directly after stmt, and block after any line
// comment that trails stmt. So the comment stays on the line of stmt.
func (lowerer *lowerer) insertAfter(stmt ast.Stmt, inline, block string, line int) {
	if inline != "" {
		lowerer.insert(stmt.End(), inline, line)
	}
	src := lowerer.file.preprocessed.Src
	offset := lowerer.offset(stmt.End())
	cursor := offset
	for cursor < len(src) && (src[cursor] == ' ' || src[cursor] == '\t') {
		cursor++
	}
	if bytes.HasPrefix(src[cursor:], []byte("//")) {
		offset = cursor + bytes.IndexByte(src[cursor:], '\n')
		if offset < cursor {
			offset = len(src)
		}
	}
	lowerer.edits = append(lowerer.edits, edit{start: offset, end: offset, text: block, line: line})
}

func (lowerer *lowerer) insertOrdered(position token.Pos, text string, line, order int) {
	offset := lowerer.offset(position)
	lowerer.edits = append(lowerer.edits, edit{start: offset, end: offset, text: text, line: line, order: order})
}

// isStatement reports whether expr, maybe in parentheses, is the expression
// of a statement.
func (lowerer *lowerer) isStatement(expr ast.Expr) bool {
	_, parent := outermostParen(lowerer.parents, expr)
	_, ok := parent.(*ast.ExprStmt)
	return ok
}

// dropParens removes the parentheses around expr, so statements can replace it.
func (lowerer *lowerer) dropParens(expr ast.Expr) {
	for paren, ok := lowerer.parents[expr].(*ast.ParenExpr); ok; paren, ok = lowerer.parents[paren].(*ast.ParenExpr) {
		line := lowerer.line(paren.Pos())
		lowerer.replace(paren.Lparen, paren.Lparen+1, "", line)
		lowerer.replace(paren.Rparen, paren.Rparen+1, "", line)
	}
}

func (lowerer *lowerer) depth(node ast.Node) int {
	depth := 0
	for ancestor := lowerer.parents[node]; ancestor != nil; ancestor = lowerer.parents[ancestor] {
		depth++
	}
	return depth
}

func (lowerer *lowerer) offset(position token.Pos) int {
	return lowerer.file.tokenFile.Offset(position)
}

func (lowerer *lowerer) replace(from, to token.Pos, text string, line int) {
	lowerer.replaceOffsets(lowerer.offset(from), lowerer.offset(to), text, line)
}

func (lowerer *lowerer) replaceOffsets(start, end int, text string, line int) {
	lowerer.edits = append(lowerer.edits, edit{start: start, end: end, text: text, line: line})
}

// drop replaces [from, to) with text and discards every edit inside it.
func (lowerer *lowerer) drop(from, to token.Pos, text string, line int) {
	lowerer.drops = append(lowerer.drops, edit{start: lowerer.offset(from), end: lowerer.offset(to), text: text, line: line})
}

func (lowerer *lowerer) line(position token.Pos) int { return lowerer.file.tokenFile.Line(position) }

// dropped reports whether the output discards the code at position. A move
// renders a hoisted call, so the drop of that call does not discard it.
func (lowerer *lowerer) dropped(position token.Pos) bool {
	offset := lowerer.offset(position)
	for _, drop := range lowerer.drops {
		if offset >= drop.start && offset < drop.end && !lowerer.isMoved(drop) {
			return true
		}
	}
	return false
}

func (lowerer *lowerer) isMoved(drop edit) bool {
	for _, candidate := range lowerer.edits {
		if candidate.move != nil && candidate.move[0] == drop.start && candidate.move[1] == drop.end {
			return true
		}
	}
	return false
}
