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
	if layer.state == "" {
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
	// The output evaluates the return values before the finally code runs.
	signature := enclosingSignature(lowerer.typeInfo, lowerer.parents, stmt)
	if signature == nil {
		return false
	}
	for _, layer := range layers {
		if !layer.terminates {
			lowerer.ensureReturnVars(layer, signature)
		}
	}
	outermost := layers[len(layers)-1]
	entry := lowerer.enter(layers, 0, "return "+strings.Join(outermost.returnVars, ", "), true)
	destination := strings.Join(layers[0].returnVars, ", ")
	if layers[0].terminates {
		// The output never returns the values but still evaluates them.
		destination = strings.TrimSuffix(strings.Repeat("_, ", signature.Results().Len()), ", ")
	}
	lowerer.replace(returnStmt.Return, returnStmt.Results[0].Pos(), "{\n"+destination+" = ", line)
	lowerer.insertAfter(stmt, "", "\n"+entry+"\n}", line)
	return true
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
	hasCatch := parts.catch != nil && !parts.isCatchDropped
	tryTerminates := isTerminating(stmt.Body)
	if hasCatch && !tryTerminates {
		layer.gotos++ // the jump over the catch block
	}
	lowerer.replace(stmt.Pos(), stmt.Body.Lbrace+1, lowerer.finallyHead(parts, hasCatch), lowerer.line(stmt.Pos()))
	lowerer.emitFinallyOpening(parts, hasCatch, tryTerminates)
	// If no path falls through the try statement, the output must not fall
	// through. Otherwise Go reports a missing return.
	isEndUnreachable := !layer.terminates && tryTerminates && (!hasCatch || isTerminating(parts.catch))
	finallyBlock := parts.finallyBlock
	lowerer.replace(finallyBlock.Rbrace, finallyBlock.Rbrace+1, finallyTail(layer, isEndUnreachable), lowerer.line(finallyBlock.Rbrace))
}

// finallyHead declares the catch variable, the pending-exit state, and the
// saved return values before the try body.
func (lowerer *lowerer) finallyHead(parts *tryStatement, hasCatch bool) string {
	layer := parts.context.finally
	var head strings.Builder
	head.WriteString("{\n")
	if hasCatch && parts.context.needsDecl {
		fmt.Fprintf(&head, "var %s error\n", parts.context.catchVar)
	}
	if layer.state != "" {
		fmt.Fprintf(&head, "var %s int\n", layer.state)
	}
	for varIndex, name := range layer.returnVars {
		typeText, _ := lowerer.typeName(layer.returnVarTypes[varIndex])
		fmt.Fprintf(&head, "var %s %s\n", name, typeText)
	}
	head.WriteString("{")
	return head.String()
}

// emitFinallyOpening rewrites the text between the try body and the finally
// body. This text holds the catch block, if kept, and the finally label.
func (lowerer *lowerer) emitFinallyOpening(parts *tryStatement, hasCatch, tryTerminates bool) {
	stmt, layer, finallyBlock := parts.stmt, parts.context.finally, parts.finallyBlock
	label := ""
	if layer.gotos > 0 {
		label = layer.label + ":\n"
	}
	opening := "}\n" + label + "{"
	bodyEnd := lowerer.line(stmt.Body.Rbrace)
	switch {
	case hasCatch:
		middle := "}\n"
		if !tryTerminates {
			middle += "goto " + layer.label + "\n"
		}
		lowerer.replaceOffsets(lowerer.offset(stmt.Body.Rbrace), parts.catchEnd, middle+parts.context.catchLabel()+":\n{"+parts.prologue, bodyEnd)
		lowerer.replace(parts.catch.Rbrace, finallyBlock.Lbrace+1, opening, lowerer.line(parts.catch.Rbrace))
	case parts.isCatchDropped:
		lowerer.drop(stmt.Body.Rbrace, finallyBlock.Lbrace+1, opening, bodyEnd)
	default:
		lowerer.replace(stmt.Body.Rbrace, finallyBlock.Lbrace+1, opening, bodyEnd)
	}
}

// finallyTail resumes each pending exit after the finally body.
func finallyTail(layer *finallyLayer, isEndUnreachable bool) string {
	var tail strings.Builder
	tail.WriteString("}\n")
	for caseIndex, caseCode := range layer.cases {
		fmt.Fprintf(&tail, "if %s == %d {\n%s\n}\n", layer.state, caseIndex+1, caseCode)
	}
	if isEndUnreachable {
		tail.WriteString("panic(\"unreachable\")\n")
	}
	tail.WriteString("}")
	return tail.String()
}
