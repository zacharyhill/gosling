package transpile

import (
	"go/ast"
	"go/token"
	"go/types"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// A try with typed catch clauses keeps the clauses as an if chain in the
// catch block. Each clause tests the hidden error variable:
//
//	catch_N:
//		{
//			if target := new(*fs.PathError); errors.As(caught, target) {
//				e := *target
//				...
//			} else if errors.Is(caught, sql.ErrNoRows) {
//				e := caught
//				...
//			} else {
//				// the catch for all errors, or the code that passes the
//				// error on to the outer try or to the caller
//			}
//		}
//
// The variables in the conditions have hidden names. So they do not hide
// names that a later clause uses.

const errUnmatched = "errors that no catch matches need an outer try or a function whose last result is error; add a last catch (e)"

// filterClause is a catch clause with a type or a value.
type filterClause struct {
	ifStmt   *ast.IfStmt
	body     []ast.Stmt // the user's code, without generated lines
	binding  string
	catchEnd int // offset after the generated prologue
	// asType is the type of catch (e: T). It is nil for catch (e is value).
	asType types.Type
	marker *ast.CallExpr
}

// addFilters reads the typed catch clauses and the last catch of a try.
func (lowerer *lowerer) addFilters(parts *tryStatement, preprocessedTry preprocess.Try) {
	try := parts.context
	try.catchVar = lowerer.fresh("caught")
	try.needsDecl = true
	try.isCatchVarRead = true // the clause conditions read it
	try.rebind = preprocessedTry.Binding
	next := parts.stmt.Else
	for _, filter := range preprocessedTry.Filters {
		ifStmt := next.(*ast.IfStmt)
		clause := &filterClause{
			ifStmt:   ifStmt,
			body:     ifStmt.Body.List[2:], // skip the generated declaration and blank use
			binding:  filter.Binding,
			catchEnd: filter.CatchEnd,
			marker:   ifStmt.Cond.(*ast.CallExpr),
		}
		if callee, ok := clause.marker.Fun.(*ast.Ident); ok && callee.Name == preprocess.AsMarker {
			clause.asType = lowerer.typeInfo.Types[clause.marker.Args[0]].Type.(*types.Pointer).Elem()
		}
		parts.filters = append(parts.filters, clause)
		next = ifStmt.Else
	}
	if preprocessedTry.CatchEnd == 0 {
		return // next is the else that prepareFilters adds
	}
	parts.catch = next.(*ast.BlockStmt)
	parts.catchBody = parts.catch.List
	if preprocessedTry.Binding != "" {
		parts.catchBody = parts.catchBody[2:]
	}
}

func (parts *tryStatement) hasCatch() bool { return parts.catch != nil || len(parts.filters) > 0 }

// lastCatchRbrace returns the closing brace of the last catch clause.
func (parts *tryStatement) lastCatchRbrace() token.Pos {
	if parts.catch != nil {
		return parts.catch.Rbrace
	}
	return parts.filters[len(parts.filters)-1].ifStmt.Body.Rbrace
}

// catchStmt returns the statement that holds all catch clauses.
func (parts *tryStatement) catchStmt() ast.Stmt {
	if len(parts.filters) > 0 {
		return parts.stmt.Else
	}
	return parts.catch
}

// emitCatches replaces the text from the end of the try body to the code
// of the first catch clause. opening starts the catch block.
func (lowerer *lowerer) emitCatches(parts *tryStatement, opening string) {
	stmt := parts.stmt
	line := lowerer.line(stmt.Body.Rbrace)
	if len(parts.filters) == 0 {
		lowerer.replaceOffsets(lowerer.offset(stmt.Body.Rbrace), parts.catchEnd, opening+parts.prologue, line)
		return
	}
	lowerer.replace(stmt.Body.Rbrace, parts.filters[0].ifStmt.Pos(), opening+"\n", line)
	errorsName, ok := lowerer.importName(lowerer.file, "errors", "errors", false)
	if !ok {
		lowerer.errorf(lowerer.file, stmt.Pos(), "typed catch needs package errors, but the name errors is already used in this file")
		return
	}
	for _, filter := range parts.filters {
		lowerer.emitFilter(parts, filter, errorsName)
	}
	if parts.catch != nil {
		lastRbrace := parts.filters[len(parts.filters)-1].ifStmt.Body.Rbrace
		lowerer.replaceOffsets(lowerer.offset(lastRbrace)+len("}"), parts.catchEnd, " else {"+parts.prologue, lowerer.line(lastRbrace))
	}
}

// emitFilter rewrites the condition and prologue of one typed catch clause.
func (lowerer *lowerer) emitFilter(parts *tryStatement, filter *filterClause, errorsName string) {
	caught, marker := parts.context.catchVar, filter.marker
	line := lowerer.line(marker.Pos())
	argument := marker.Args[0]
	isBindingUsed := lowerer.usesObject(filter.body, lowerer.findFirstDeclared(filter.ifStmt.Body))
	prologue := ""
	if filter.asType == nil {
		if isBindingUsed {
			prologue = "\n" + filter.binding + " := " + caught
		}
		lowerer.replace(marker.Pos(), argument.Pos(), errorsName+".Is("+caught+", ", line)
		lowerer.replaceOffsets(lowerer.offset(argument.End()), filter.catchEnd, ") {"+prologue, line)
		return
	}
	if !types.IsInterface(filter.asType) && !types.Implements(filter.asType, errorType.Underlying().(*types.Interface)) {
		typeExpr := argument.(*ast.CallExpr).Args[0]
		lowerer.errorf(lowerer.file, typeExpr.Pos(), "catch type %s does not implement error", lowerer.file.text(typeExpr))
		return
	}
	target := lowerer.fresh("target")
	if isBindingUsed {
		prologue = "\n" + filter.binding + " := *" + target
	}
	lowerer.replace(marker.Pos(), argument.Pos(), target+" := ", line)
	lowerer.replaceOffsets(lowerer.offset(argument.End()), filter.catchEnd, "; "+errorsName+".As("+caught+", "+target+") {"+prologue, line)
}

// buildCatchChainEnd returns the code after the last typed catch clause.
// Without a catch for all errors, an else passes the error on. It goes to
// the outer try, after the finally block of this try. Without an outer try,
// the function returns it.
func (lowerer *lowerer) buildCatchChainEnd(parts *tryStatement) string {
	if parts.catch != nil {
		return "\n}"
	}
	try := parts.context
	from := &tryContext{isCatchBody: true, finally: try.finally, outer: try.outer}
	if target := from.target(); target != nil {
		target.jumps++
		return " else {\n" + lowerer.failTo(from, target, try.catchVar) + "\n}\n}"
	}
	signature := enclosingSignature(lowerer.typeInfo, lowerer.parents, parts.stmt)
	if signature == nil || !returnsError(signature) {
		lowerer.errorf(lowerer.file, parts.stmt.Pos(), errUnmatched)
		return ""
	}
	zeros, ok := lowerer.buildZeroResults(signature, parts.stmt, "typed catch")
	if !ok {
		return ""
	}
	values := zeros + try.catchVar
	exit := "return " + values
	if layers := lowerer.crossed(nil, from); len(layers) > 0 {
		destination, entry := lowerer.prepareFinallyReturn(layers, signature)
		exit = destination + " = " + values + "\n" + entry
	}
	return " else {\n" + exit + "\n}\n}"
}

// prepareFilters gives each try whose catch clauses all have a filter an
// else branch that panics. In the output, this branch passes the error on
// and never completes. So the type checker sees the same terminating
// statements as in the output.
func (file *file) prepareFilters() {
	filterCountByTryOffset := map[int]int{}
	for _, preprocessedTry := range file.preprocessed.Tries {
		if len(preprocessedTry.Filters) > 0 && preprocessedTry.CatchEnd == 0 {
			filterCountByTryOffset[preprocessedTry.If] = len(preprocessedTry.Filters)
		}
	}
	if len(filterCountByTryOffset) == 0 {
		return
	}
	ast.Inspect(file.ast, func(node ast.Node) bool {
		ifStmt, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		filterCount, ok := filterCountByTryOffset[file.tokenFile.Offset(ifStmt.Pos())]
		if !ok {
			return true
		}
		lastFilter := ifStmt
		for range filterCount {
			lastFilter = lastFilter.Else.(*ast.IfStmt)
		}
		panicCall := &ast.CallExpr{Fun: ast.NewIdent("panic"), Args: []ast.Expr{&ast.BasicLit{Kind: token.STRING, Value: `""`}}}
		lastFilter.Else = &ast.BlockStmt{List: []ast.Stmt{&ast.ExprStmt{X: panicCall}}}
		return true
	})
}
