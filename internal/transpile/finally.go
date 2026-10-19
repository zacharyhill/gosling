package transpile

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strings"
)

// finallyLayer lowers one finally block. The block appears once in the
// output, under a label after the catch block. An exit that leaves the try
// statement records its destination in a state variable. Exits are return,
// break, continue, goto, or a failure that an outer try catches. The exit
// jumps to the label and resumes after the finally body:
//
//	{
//		var fin int
//		var ret T
//		{ ... ret = value; fin = 1; goto finally_1 ... }
//	finally_1:
//		{ finally body }
//		if fin == 1 {
//			return ret
//		}
//	}
type finallyLayer struct {
	label          string
	state          string // the pending exit, empty until an exit needs it
	returnVars     []string
	returnVarTypes []types.Type
	cases          []string // code that resumes exit i+1 after the finally body
	gotos          int
	// terminates is true if the finally body always returns or panics. Then
	// no exit resumes after it.
	terminates bool
	start, end token.Pos // the whole try statement, finally block included
	// A guard layer has no finally body. Its exits return from the function
	// literal of a guarded try body. Its start and end are the try body.
	isGuard bool
}

// prepareFinally moves each finally block in front of its try statement in
// the AST. It gives a try without catch an else branch that panics. Then the
// type checker treats a try whose branches all return as terminating, as in
// the output.
func (file *file) prepareFinally() {
	finallyToTry := map[int]int{}
	for _, preprocessedTry := range file.preprocessed.Tries {
		if preprocessedTry.Finally != 0 {
			finallyToTry[preprocessedTry.Finally] = preprocessedTry.If
		}
	}
	if len(finallyToTry) == 0 {
		return
	}
	ast.Inspect(file.ast, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.BlockStmt:
			file.moveFinallyBlocks(node.List, finallyToTry)
		case *ast.CaseClause:
			file.moveFinallyBlocks(node.Body, finallyToTry)
		case *ast.CommClause:
			file.moveFinallyBlocks(node.Body, finallyToTry)
		}
		return true
	})
}

// moveFinallyBlocks swaps each finally block in list with the try
// statement directly before it.
func (file *file) moveFinallyBlocks(list []ast.Stmt, finallyToTry map[int]int) {
	for stmtIndex := 1; stmtIndex < len(list); stmtIndex++ {
		block, ok := list[stmtIndex].(*ast.BlockStmt)
		if !ok {
			continue
		}
		tryOffset, ok := finallyToTry[file.tokenFile.Offset(block.Lbrace)]
		tryStmt, isIf := list[stmtIndex-1].(*ast.IfStmt)
		if !ok || !isIf || file.tokenFile.Offset(tryStmt.Pos()) != tryOffset {
			continue
		}
		list[stmtIndex-1], list[stmtIndex] = block, tryStmt
		file.finallyOf[tryStmt] = block
		file.finallyBlocks[block] = true
		if tryStmt.Else == nil {
			panicCall := &ast.CallExpr{Fun: ast.NewIdent("panic"), Args: []ast.Expr{&ast.BasicLit{Kind: token.STRING, Value: `""`}}}
			tryStmt.Else = &ast.BlockStmt{List: []ast.Stmt{&ast.ExprStmt{X: panicCall}}}
		}
	}
}

func (lowerer *lowerer) stateVar(layer *finallyLayer) string {
	if layer.state == "" && layer.isGuard {
		layer.state = lowerer.fresh("exit")
	} else if layer.state == "" {
		layer.state = lowerer.fresh("fin")
	}
	return layer.state
}

func (lowerer *lowerer) ensureReturnVars(layer *finallyLayer, signature *types.Signature) {
	if layer.returnVars != nil {
		return
	}
	for resultVar := range signature.Results().Variables() {
		layer.returnVars = append(layer.returnVars, lowerer.fresh("ret"))
		layer.returnVarTypes = append(layer.returnVarTypes, resultVar.Type())
	}
}

// enter registers an exit with layers[layerIndex:] and returns the code that
// starts it. This code records the exit and jumps to the innermost finally.
// The resume code of each layer passes the exit to the next layer. The last
// layer runs final.
func (lowerer *lowerer) enter(layers []*finallyLayer, layerIndex int, final string, carriesValues bool) string {
	layer := layers[layerIndex]
	if layer.terminates {
		layer.gotos++
		return "goto " + layer.label
	}
	body := final
	if layerIndex+1 < len(layers) {
		next := layers[layerIndex+1]
		body = lowerer.enter(layers, layerIndex+1, final, carriesValues)
		if carriesValues && !next.terminates {
			body = strings.Join(next.returnVars, ", ") + " = " + strings.Join(layer.returnVars, ", ") + "\n" + body
		}
	}
	caseNumber := slices.Index(layer.cases, body) + 1 // exits that resume the same way share a case
	if caseNumber == 0 {
		layer.cases = append(layer.cases, body)
		caseNumber = len(layer.cases)
	}
	if layer.isGuard {
		return fmt.Sprintf("%s = %d\nreturn", lowerer.stateVar(layer), caseNumber)
	}
	layer.gotos++
	return fmt.Sprintf("%s = %d\ngoto %s", lowerer.stateVar(layer), caseNumber, layer.label)
}

// crossed returns the finally layers, innermost first, that an exit at stmt
// leaves. A return leaves all of them. A branch leaves the layers that do
// not contain its target.
func (lowerer *lowerer) crossed(stmt ast.Stmt, try *tryContext) []*finallyLayer {
	var target ast.Node
	if branch, ok := stmt.(*ast.BranchStmt); ok {
		if target = lowerer.branchTarget(branch); target == nil {
			return nil
		}
	}
	var layers []*finallyLayer
	for current := try; current != nil; current = current.outer {
		layer := current.finally
		if layer == nil || (len(layers) > 0 && layers[len(layers)-1] == layer) {
			continue
		}
		if target != nil && target.Pos() >= layer.start && target.End() <= layer.end {
			continue
		}
		layers = append(layers, layer)
		if layer.terminates {
			break // the exit never gets past this finally body
		}
	}
	return layers
}

// branchTarget returns the statement a break, continue, or goto refers to.
func (lowerer *lowerer) branchTarget(branch *ast.BranchStmt) ast.Node {
	if branch.Tok == token.GOTO {
		return lowerer.findLabel(branch, branch.Label.Name)
	}
	for ancestor := lowerer.parents[branch]; ancestor != nil; ancestor = lowerer.parents[ancestor] {
		switch ancestor := ancestor.(type) {
		case *ast.FuncLit, *ast.FuncDecl:
			return nil
		case *ast.LabeledStmt:
			if branch.Label != nil && ancestor.Label.Name == branch.Label.Name {
				return ancestor
			}
		case *ast.ForStmt, *ast.RangeStmt:
			if branch.Label == nil && (branch.Tok == token.BREAK || branch.Tok == token.CONTINUE) {
				return ancestor
			}
		case *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
			if branch.Label == nil && branch.Tok == token.BREAK {
				return ancestor
			}
		}
	}
	return nil
}

func (lowerer *lowerer) findLabel(from ast.Node, name string) ast.Node {
	var body ast.Node
	for ancestor := lowerer.parents[from]; ancestor != nil && body == nil; ancestor = lowerer.parents[ancestor] {
		switch ancestor := ancestor.(type) {
		case *ast.FuncLit:
			body = ancestor.Body
		case *ast.FuncDecl:
			body = ancestor.Body
		}
	}
	var found ast.Node
	ast.Inspect(body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.LabeledStmt:
			if node.Label.Name == name {
				found = node
			}
		}
		return found == nil
	})
	return found
}

// lowerExit sends a return or branch that leaves a try with a finally block
// through the finally code. It reports whether stmt is such an exit.
func (lowerer *lowerer) lowerExit(stmt ast.Stmt, try *tryContext) bool {
	switch stmt.(type) {
	case *ast.ReturnStmt, *ast.BranchStmt:
	default:
		return false
	}
	layers := lowerer.crossed(stmt, try)
	if len(layers) == 0 {
		return false
	}
	line := lowerer.line(stmt.Pos())
	returnStmt, isReturn := stmt.(*ast.ReturnStmt)
	if !isReturn || len(returnStmt.Results) == 0 {
		final := lowerer.file.text(stmt)
		lowerer.replace(stmt.Pos(), stmt.End(), "{\n"+lowerer.enter(layers, 0, final, false)+"\n}", line)
		return true
	}
	signature := enclosingSignature(lowerer.typeInfo, lowerer.parents, stmt)
	if signature == nil {
		return false
	}
	destination, entry := lowerer.prepareFinallyReturn(layers, signature)
	lowerer.replace(returnStmt.Return, returnStmt.Results[0].Pos(), "{\n"+destination+" = ", line)
	lowerer.insertAfter(stmt, "", "\n"+entry+"\n}", line)
	return true
}

// prepareFinallyReturn registers a return with values that leaves layers. It
// returns the variables that receive the values, and the code that then
// runs the finally blocks and returns.
func (lowerer *lowerer) prepareFinallyReturn(layers []*finallyLayer, signature *types.Signature) (destination, entry string) {
	for _, layer := range layers {
		if !layer.terminates {
			lowerer.ensureReturnVars(layer, signature)
		}
	}
	outermost := layers[len(layers)-1]
	entry = lowerer.enter(layers, 0, "return "+strings.Join(outermost.returnVars, ", "), true)
	destination = strings.Join(layers[0].returnVars, ", ")
	if layers[0].terminates {
		// The output never returns the values but still evaluates them.
		destination = strings.TrimSuffix(strings.Repeat("_, ", signature.Results().Len()), ", ")
	}
	return destination, entry
}

// jumpOut returns the code for a failure in try that target catches. The
// code goes through each finally layer between them, then to the catch
// block of target.
func (lowerer *lowerer) jumpOut(try, target *tryContext, final string) string {
	var layers []*finallyLayer
	for current := try; current != nil && current != target; current = current.outer {
		if current.finally != nil && (len(layers) == 0 || layers[len(layers)-1] != current.finally) {
			layers = append(layers, current.finally)
		}
	}
	if len(layers) == 0 {
		return final
	}
	return lowerer.enter(layers, 0, final, false)
}

// emitFinally replaces the try, catch, and finally markers for a try
// statement with a finally block.
func (lowerer *lowerer) emitFinally(parts *tryStatement) {
	stmt, layer := parts.stmt, parts.context.finally
	for _, returnType := range layer.returnVarTypes {
		typeText, _ := lowerer.typeName(returnType)
		lowerer.reportHiddenType(returnType, typeText, stmt.Pos())
	}
	lowerer.replace(stmt.Pos(), stmt.Body.Lbrace+1, lowerer.finallyHead(parts), lowerer.line(stmt.Pos()))
	lowerer.emitFinallyOpening(parts)
	// If no path falls through the try statement, the output must not fall
	// through. Otherwise Go reports a missing return.
	isEndUnreachable := !layer.terminates && isTerminating(stmt.Body)
	for _, current := range parts.collectHandlers() {
		isEndUnreachable = isEndUnreachable && isTerminating(current.stmt)
	}
	finallyBlock := parts.finallyBlock
	if isEndUnreachable {
		lowerer.checkBuiltins(finallyBlock.Rbrace, "finally", "panic")
	}
	lowerer.replace(finallyBlock.Rbrace, finallyBlock.Rbrace+1, finallyTail(layer, isEndUnreachable), lowerer.line(finallyBlock.Rbrace))
}

// finallyHead declares the catch variable, the pending-exit state, and the
// saved return values before the try body.
func (lowerer *lowerer) finallyHead(parts *tryStatement) string {
	layer := parts.context.finally
	var head strings.Builder
	head.WriteString("{\n")
	if parts.hasCatch() && !parts.isCatchDropped && parts.context.needsDecl {
		fmt.Fprintf(&head, "var %s error\n", parts.context.catchVar)
		lowerer.checkBuiltins(parts.stmt.Pos(), "try", "error")
	}
	if layer.state != "" {
		lowerer.checkBuiltins(parts.stmt.Pos(), "finally", "int")
	}
	lowerer.writeLayerDecls(&head, layer)
	head.WriteString(lowerer.guardDecls(parts))
	head.WriteString(lowerer.bodyOpen(parts))
	return head.String()
}

// writeLayerDecls declares the state and the saved return values of layer.
func (lowerer *lowerer) writeLayerDecls(decls *strings.Builder, layer *finallyLayer) {
	if layer.state != "" {
		fmt.Fprintf(decls, "var %s int\n", layer.state)
	}
	for varIndex, name := range layer.returnVars {
		typeText, _ := lowerer.typeName(layer.returnVarTypes[varIndex])
		fmt.Fprintf(decls, "var %s %s\n", name, typeText)
	}
}

// emitFinallyOpening rewrites the text between the try body and the finally
// body. This text holds the handlers, if kept, and the finally label.
func (lowerer *lowerer) emitFinallyOpening(parts *tryStatement) {
	stmt, layer, finallyBlock := parts.stmt, parts.context.finally, parts.finallyBlock
	from := stmt.Body.Rbrace
	hasHandlers := len(parts.collectHandlers()) > 0
	if hasHandlers {
		jumps, lastRbrace := lowerer.emitHandlers(parts, layer.label)
		layer.gotos += jumps
		from = lastRbrace
	}
	opening := "}" + parts.chainEnd + "\n{"
	if layer.gotos > 0 {
		opening = "}" + parts.chainEnd + "\n" + layer.label + ":\n{"
	}
	if !hasHandlers && parts.isCatchDropped {
		lowerer.drop(from, finallyBlock.Lbrace+1, opening, lowerer.line(from))
		return
	}
	lowerer.replace(from, finallyBlock.Lbrace+1, opening, lowerer.line(from))
}

// finallyTail resumes each pending exit after the finally body.
func finallyTail(layer *finallyLayer, isEndUnreachable bool) string {
	var tail strings.Builder
	tail.WriteString("}\n")
	writeExitCases(&tail, layer)
	if isEndUnreachable {
		tail.WriteString("panic(\"unreachable\")\n")
	}
	tail.WriteString("}")
	return tail.String()
}

// writeExitCases writes the code that resumes each pending exit of layer.
func writeExitCases(code *strings.Builder, layer *finallyLayer) {
	for caseIndex, caseCode := range layer.cases {
		fmt.Fprintf(code, "if %s == %d {\n%s\n}\n", layer.state, caseIndex+1, caseCode)
	}
}
