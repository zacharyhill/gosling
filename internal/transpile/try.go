package transpile

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strings"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

const errUncheckableCall = "this call can fail, but its error cannot be checked here; move it into its own statement"

const errConditionalCall = "this call can fail but only runs conditionally here, so its error cannot be checked; move it into its own statement"

type tryContext struct {
	id         int
	catchVar   string
	hasBinding bool
	jumps      int
	needsDecl  bool
	// If generated code reads catchVar, catchVar needs no blank use.
	isCatchVarRead bool
	// rebind is the user's catch variable name if catchVar is a hidden
	// stand-in for it.
	rebind string
	// A catch body context reserves the name of the catch variable.
	// It is not a jump target.
	isCatchBody bool
	// A try with only a finally block is not a jump target. Failures go
	// to an enclosing try.
	isFinallyOnly bool
	finally       *finallyLayer
	outer         *tryContext
}

// target returns the try block that a failing call jumps to, if any.
func (tryContext *tryContext) target() *tryContext {
	for ; tryContext != nil; tryContext = tryContext.outer {
		if !tryContext.isCatchBody && !tryContext.isFinallyOnly {
			return tryContext
		}
	}
	return nil
}

func (tryContext *tryContext) catchLabel() string { return fmt.Sprintf("catch_%d", tryContext.id) }

func (tryContext *tryContext) endLabel() string { return fmt.Sprintf("end_%d", tryContext.id) }

func (tryContext *tryContext) finallyLabel() string { return fmt.Sprintf("finally_%d", tryContext.id) }

// callSite is a statement-level call: a bare call or the single right side
// of an assignment.
type callSite struct {
	call     ast.Expr
	assign   *ast.AssignStmt
	leftSide []ast.Expr // original LHS, without the blank added for type checking
}

func (lowerer *lowerer) callSite(stmt ast.Stmt) (callSite, bool) {
	switch stmt := stmt.(type) {
	case *ast.ExprStmt:
		return callSite{call: stmt.X}, true
	case *ast.AssignStmt:
		if len(stmt.Rhs) != 1 || (stmt.Tok != token.DEFINE && stmt.Tok != token.ASSIGN) {
			return callSite{}, false
		}
		leftSide := stmt.Lhs
		if originalCount, ok := lowerer.file.augmented[stmt]; ok {
			leftSide = leftSide[:originalCount]
		}
		return callSite{call: stmt.Rhs[0], assign: stmt, leftSide: leftSide}, true
	}
	return callSite{}, false
}

// tryStatement holds the pieces of one try statement during lowering.
type tryStatement struct {
	stmt           *ast.IfStmt
	catch          *ast.BlockStmt // nil without a catch clause
	catchBody      []ast.Stmt     // the user's catch code, without generated lines
	catchEnd       int            // offset after the generated catch prologue
	finallyBlock   *ast.BlockStmt // nil without a finally clause
	context        *tryContext
	prologue       string // code that starts the catch block
	isCatchDropped bool
}

func (lowerer *lowerer) lowerTry(stmt *ast.IfStmt, preprocessedTry preprocess.Try, outerTry *tryContext) {
	parts := lowerer.newTryStatement(stmt, preprocessedTry, outerTry)
	lowerer.walkTryStatement(parts, preprocessedTry.Binding, outerTry)
	// If nothing in the try can fail, the catch block never runs.
	parts.isCatchDropped = parts.catch != nil && parts.context.jumps == 0
	parts.prologue = lowerer.catchPrologue(parts)
	switch {
	case parts.finallyBlock != nil:
		lowerer.emitFinally(parts)
	case parts.isCatchDropped:
		lowerer.emitWithoutCatch(parts)
	default:
		lowerer.emitTry(parts)
	}
}

func (lowerer *lowerer) newTryStatement(stmt *ast.IfStmt, preprocessedTry preprocess.Try, outerTry *tryContext) *tryStatement {
	binding := preprocessedTry.Binding
	parts := &tryStatement{
		stmt:         stmt,
		catchEnd:     preprocessedTry.CatchEnd,
		finallyBlock: lowerer.file.finallyOf[stmt],
		context: &tryContext{
			id:            lowerer.nextTryID(),
			catchVar:      binding,
			hasBinding:    binding != "",
			needsDecl:     binding != "",
			isFinallyOnly: !preprocessedTry.HasCatch,
			outer:         outerTry,
		},
	}
	if parts.finallyBlock != nil {
		parts.context.finally = &finallyLayer{
			label:      parts.context.finallyLabel(),
			start:      stmt.Pos(),
			end:        parts.finallyBlock.End(),
			terminates: isTerminating(parts.finallyBlock),
		}
	}
	if preprocessedTry.HasCatch {
		parts.catch, _ = stmt.Else.(*ast.BlockStmt)
		parts.catchBody = parts.catch.List
	}
	if parts.context.hasBinding {
		parts.catchBody = parts.catchBody[2:] // skip the generated declaration and blank use
		lowerer.hideShadowedBinding(parts)
	}
	return parts
}

// hideShadowedBinding handles a try block that uses the catch variable's
// name for something else. The output declares the error variable outside
// the try block, so it gets a hidden name. The catch block binds the user's name.
func (lowerer *lowerer) hideShadowedBinding(parts *tryStatement) {
	binding := parts.context.catchVar
	if _, ok := lowerer.usedIn(binding, parts.stmt.Body.Lbrace, parts.stmt.Body.Rbrace); ok {
		parts.context.rebind = binding
		parts.context.catchVar = lowerer.fresh("caught")
	}
}

func (lowerer *lowerer) walkTryStatement(parts *tryStatement, binding string, outerTry *tryContext) {
	lowerer.walkBlock(parts.stmt.Body.List, parts.context)
	if parts.catch != nil {
		catchContext := &tryContext{catchVar: binding, isCatchBody: true, finally: parts.context.finally, outer: outerTry}
		lowerer.walkBlock(parts.catchBody, catchContext)
	}
	if parts.finallyBlock != nil {
		lowerer.walkBlock(parts.finallyBlock.List, outerTry) // the finally code runs outside this try
	}
}

// catchPrologue ends in a semicolon, because user code can follow it on the
// same line.
func (lowerer *lowerer) catchPrologue(parts *tryStatement) string {
	try := parts.context
	isBindingUsed := lowerer.usesObject(parts.catchBody, lowerer.findBindingObject(parts))
	switch {
	case try.rebind != "" && isBindingUsed:
		return "\n" + try.rebind + " := " + try.catchVar + ";"
	case try.needsDecl && !try.isCatchVarRead && !isBindingUsed:
		return "\n_ = " + try.catchVar + ";"
	}
	return ""
}

// emitWithoutCatch keeps the try body as a plain block and drops the catch
// block and all its contents.
func (lowerer *lowerer) emitWithoutCatch(parts *tryStatement) {
	stmt := parts.stmt
	lowerer.replace(stmt.Pos(), stmt.Body.Lbrace+1, "{", lowerer.line(stmt.Pos()))
	lowerer.drop(stmt.Body.Rbrace, parts.catch.Rbrace+1, "}", lowerer.line(stmt.Body.Rbrace))
}

// emitTry replaces the try/catch markers with labeled blocks:
//
//	{ var e error; { try body }; goto end_N; catch_N: { catch body }; end_N: }
func (lowerer *lowerer) emitTry(parts *tryStatement) {
	stmt, try := parts.stmt, parts.context
	head := "{\n{"
	if try.needsDecl {
		head = fmt.Sprintf("{\nvar %s error\n{", try.catchVar)
		lowerer.checkBuiltins(stmt.Pos(), "try", "error")
	}
	lowerer.replace(stmt.Pos(), stmt.Body.Lbrace+1, head, lowerer.line(stmt.Pos()))

	// Skip the jump over the catch block if the try block cannot complete. The jump
	// would fail Go's terminating statement check, and go vet reports it as unreachable.
	canBodyComplete := !neverCompletes(stmt.Body, neverReturns(lowerer.typeInfo))
	var middle strings.Builder
	middle.WriteString("}\n")
	if canBodyComplete {
		fmt.Fprintf(&middle, "goto %s\n", try.endLabel())
	}
	fmt.Fprintf(&middle, "%s:\n{", try.catchLabel())
	middle.WriteString(parts.prologue)
	lowerer.replaceOffsets(lowerer.offset(stmt.Body.Rbrace), parts.catchEnd, middle.String(), lowerer.line(stmt.Body.Rbrace))

	tail := "}\n}"
	if canBodyComplete {
		tail = fmt.Sprintf("}\n%s:\n}", try.endLabel())
	}
	lowerer.replace(parts.catch.Rbrace, parts.catch.Rbrace+1, tail, lowerer.line(parts.catch.Rbrace))
}

// findBindingObject returns the catch variable from the declaration that the
// preprocessor puts first in a catch block with a binding.
func (lowerer *lowerer) findBindingObject(parts *tryStatement) types.Object {
	if parts.catch == nil || !parts.context.hasBinding || len(parts.catch.List) == 0 {
		return nil
	}
	decl, ok := parts.catch.List[0].(*ast.DeclStmt)
	if !ok {
		return nil
	}
	genDecl, ok := decl.Decl.(*ast.GenDecl)
	if !ok || len(genDecl.Specs) == 0 {
		return nil
	}
	spec, ok := genDecl.Specs[0].(*ast.ValueSpec)
	if !ok || len(spec.Names) == 0 {
		return nil
	}
	return lowerer.typeInfo.Defs[spec.Names[0]]
}

func (lowerer *lowerer) usesObject(list []ast.Stmt, object types.Object) bool {
	if object == nil {
		return false
	}
	found := false
	for _, stmt := range list {
		ast.Inspect(stmt, func(node ast.Node) bool {
			if ident, ok := node.(*ast.Ident); ok && lowerer.typeInfo.Uses[ident] == object {
				found = true
			}
			return !found
		})
	}
	return found
}

// checkBare turns a bare call into `if _, err := call; err != nil { body }`.
func (lowerer *lowerer) checkBare(stmt ast.Stmt, resultCount int, errVar, body string) {
	line := lowerer.line(stmt.Pos())
	lowerer.insert(stmt.Pos(), "if "+blanks(resultCount-1)+errVar+" := ", line)
	lowerer.insertAfter(stmt, fmt.Sprintf("; %s != nil {", errVar), "\n"+body+"\n}", line)
}

// checkAssign appends errVar to an assignment's LHS and checks it after.
func (lowerer *lowerer) checkAssign(stmt ast.Stmt, site callSite, errVar, body string) {
	line := lowerer.line(stmt.Pos())
	lowerer.insert(site.leftSide[len(site.leftSide)-1].End(), ", "+errVar, line)
	lowerer.insertAfter(stmt, "", fmt.Sprintf("\nif %s != nil {\n%s\n}", errVar, body), line)
}

// lowerTryStmt turns a fallible statement inside a try block into a check
// that jumps to the catch block. It reports whether it added a check.
func (lowerer *lowerer) lowerTryStmt(stmt ast.Stmt, try *tryContext) bool {
	target := try.target()
	if target == nil {
		return false
	}
	site, resultCount, ok := lowerer.checkableCall(stmt)
	if !ok || !lowerer.checkBuiltins(stmt.Pos(), "try", "nil") {
		return false
	}
	target.jumps++
	lowerer.checkedAssigns[site.assign] = true
	jump := func(errVar string) string { return lowerer.failTo(try, target, errVar) }
	errorResult := lastResultType(lowerer.typeInfo, site.call)
	switch {
	case site.assign == nil:
		errVar := lowerer.pickErrorVar(try, nil, token.NoPos, token.NoPos, errorResult)
		lowerer.checkBare(stmt, resultCount, errVar, jump(errVar))
	case site.assign.Tok == token.DEFINE:
		errVar := lowerer.pickErrorVar(try, lowerer.scopeAt(stmt.Pos()), stmt.Pos(), token.NoPos, errorResult)
		lowerer.checkAssign(stmt, site, errVar, jump(errVar))
	case !types.Identical(errorResult, errorType):
		lowerer.checkTypedAssign(stmt, site, resultCount, try, target)
	default:
		// A plain assignment cannot declare a temporary, so the error goes
		// directly into the catch variable.
		if target.catchVar == "" {
			target.catchVar = lowerer.fresh("tryErr")
			target.needsDecl = true
		}
		target.isCatchVarRead = true
		lowerer.checkAssign(stmt, site, target.catchVar, jump(target.catchVar))
	}
	return true
}

// checkTypedAssign checks a plain assignment whose error result has a type
// such as *MyError. The catch variable has type error, and a nil *MyError in
// it is not nil. So the check compares the typed result first:
//
//	val, err := f(); x = val; if err != nil { ...jump to catch... }
func (lowerer *lowerer) checkTypedAssign(stmt ast.Stmt, site callSite, resultCount int, try, target *tryContext) {
	anchor := lowerer.withLabels(stmt)
	valueVars := make([]string, resultCount-1)
	for valueIndex := range valueVars {
		valueVars[valueIndex] = lowerer.fresh("val")
	}
	valueList := strings.Join(valueVars, ", ")
	errVar := lowerer.pickErrorVar(try, lowerer.scopeAt(anchor.Pos()), anchor.Pos(), token.NoPos, lastResultType(lowerer.typeInfo, site.call))
	line := lowerer.line(stmt.Pos())
	offset := lowerer.offset(anchor.Pos())
	lowerer.edits = append(lowerer.edits, edit{
		start:  offset,
		end:    offset,
		line:   line,
		move:   &[2]int{lowerer.offset(site.call.Pos()), lowerer.offset(site.call.End())},
		before: valueList + ", " + errVar + " := ",
		after:  "\n",
	})
	// The call ends the statement. An insertion after the statement would
	// also be inside the moved call, so the check replaces the call.
	checkCode := fmt.Sprintf("%s\nif %s != nil {\n%s\n}", valueList, errVar, lowerer.failTo(try, target, errVar))
	lowerer.drop(site.call.Pos(), site.call.End(), checkCode, line)
}

// checkableCall returns the fallible call in stmt that try should check,
// with its result count. It adds a diagnostic for a call whose error try
// cannot check.
func (lowerer *lowerer) checkableCall(stmt ast.Stmt) (callSite, int, bool) {
	site, ok := lowerer.callSite(stmt)
	if !ok {
		return callSite{}, 0, false
	}
	if site.assign == nil && errorBeforeLast(lowerer.typeInfo, site.call) {
		callee := ast.Unparen(site.call).(*ast.CallExpr).Fun
		lowerer.errorf(lowerer.file, stmt.Pos(), "error is not the last result of %s, so try cannot check it; assign the results and check the error explicitly", lowerer.file.text(callee))
		return callSite{}, 0, false
	}
	resultCount, ok := fallible(lowerer.typeInfo, site.call)
	if !ok || (site.assign != nil && len(site.leftSide) != resultCount-1) {
		return callSite{}, 0, false
	}
	if lowerer.optionalRoots[ast.Unparen(site.call)] {
		lowerer.checkedAssigns[site.assign] = true
		lowerer.errorf(lowerer.file, stmt.Pos(), "the error from an optional call is not checked inside try; use an explicit nil check")
		return callSite{}, 0, false
	}
	return site, resultCount, true
}

// reportUncheckedAssigns reports each assignment that got a blank error for
// type checking but no check. Its output would not compile.
func (lowerer *lowerer) reportUncheckedAssigns() {
	var uncheckedCalls []*ast.CallExpr
	for assign := range lowerer.file.augmented {
		if lowerer.checkedAssigns[assign] {
			continue
		}
		if call, ok := ast.Unparen(assign.Rhs[0]).(*ast.CallExpr); ok {
			uncheckedCalls = append(uncheckedCalls, call)
		}
	}
	slices.SortFunc(uncheckedCalls, func(left, right *ast.CallExpr) int { return cmp.Compare(left.Pos(), right.Pos()) })
	for _, call := range uncheckedCalls {
		lowerer.errorf(lowerer.file, call.Pos(), "%s returns an error that is not checked here; assign the error, or move the call into its own statement in a try block", lowerer.file.text(call.Fun))
	}
}

func blanks(count int) string { return strings.Repeat("_, ", count) }

// addBlankForErrors turns `x := f()` into `x, _ := f()` if f returns
// (T, error). Then the type checker accepts it.
func addBlankForErrors(file *file, typeInfo *types.Info) bool {
	changed := false
	ast.Inspect(file.ast, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 || (assign.Tok != token.DEFINE && assign.Tok != token.ASSIGN) {
			return true
		}
		if _, done := file.augmented[assign]; done {
			return true
		}
		resultCount, ok := fallible(typeInfo, assign.Rhs[0])
		if ok && len(assign.Lhs) == resultCount-1 {
			file.augmented[assign] = len(assign.Lhs)
			assign.Lhs = append(assign.Lhs, ast.NewIdent("_"))
			changed = true
		}
		return true
	})
	return changed
}

var errorType = types.Universe.Lookup("error").Type()

var errorInterface = errorType.Underlying().(*types.Interface)

// isErrorResult reports whether a last result of type resultType counts as
// a failure. Besides error, a pointer or an interface that implements error
// counts, such as *MyError. Other types that implement error cannot be nil.
func isErrorResult(resultType types.Type) bool {
	if types.Identical(resultType, errorType) {
		return true
	}
	if _, isTypeParam := types.Unalias(resultType).(*types.TypeParam); isTypeParam {
		return false
	}
	switch resultType.Underlying().(type) {
	case *types.Pointer, *types.Interface:
		return types.Implements(resultType, errorInterface)
	}
	return false
}

// lastResultType returns the type of the last result of the call expr.
func lastResultType(typeInfo *types.Info, expr ast.Expr) types.Type {
	resultType := typeInfo.Types[expr].Type
	if tuple, ok := resultType.(*types.Tuple); ok {
		return tuple.At(tuple.Len() - 1).Type()
	}
	return resultType
}

// fallible reports whether expr is a call whose last result is an error result,
// and how many results it has.
func fallible(typeInfo *types.Info, expr ast.Expr) (int, bool) {
	call, ok := ast.Unparen(expr).(*ast.CallExpr)
	if !ok {
		return 0, false
	}
	if ident, ok := call.Fun.(*ast.Ident); ok && strings.HasPrefix(ident.Name, arrayMarkerPrefix) {
		return 0, false // a find over errors returns an element, not a failure
	}
	switch resultType := typeInfo.Types[expr].Type.(type) {
	case *types.Tuple:
		return resultType.Len(), resultType.Len() > 0 && isErrorResult(resultType.At(resultType.Len()-1).Type())
	case nil:
		return 0, false
	default:
		return 1, isErrorResult(resultType)
	}
}

// errorBeforeLast reports whether expr is a call with an error result before
// its last result. try cannot check such an error.
func errorBeforeLast(typeInfo *types.Info, expr ast.Expr) bool {
	tuple, ok := typeInfo.Types[expr].Type.(*types.Tuple)
	if !ok {
		return false
	}
	for resultIndex := range tuple.Len() - 1 {
		if types.Identical(tuple.At(resultIndex).Type(), errorType) {
			return true
		}
	}
	return false
}
