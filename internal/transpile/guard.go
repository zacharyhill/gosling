package transpile

import (
	"fmt"
	"go/ast"
	"strings"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// guard runs the body of a try with catch panic in a function literal with
// a deferred recover. An exit from the body records itself in a state
// variable and returns from the literal. After the literal, the exit
// resumes, as after a finally block:
//
//	{
//		var recovered any
//		var exit int
//		{
//			func() {
//				defer func() {
//					recovered = recover()
//				}()
//				... exit = 1; return ...
//			}()
//			if exit == 1 {
//				goto catch_1
//			}
//			if recovered != nil {
//				goto panic_1
//			}
//		}
//		...
//	panic_1:
//		{ catch panic body }
//	}
type guard struct {
	layer     *finallyLayer
	context   *tryContext // the context of the try body
	recovered string
	binding   string // the user's catch panic variable, or empty
}

func (lowerer *lowerer) attachGuard(parts *tryStatement, block *ast.BlockStmt, preprocessedTry preprocess.Try) {
	layer := &finallyLayer{isGuard: true, start: parts.stmt.Body.Lbrace, end: parts.stmt.Body.End()}
	parts.panicBlock = block
	parts.panicBody = block.List
	parts.panicEnd = preprocessedTry.PanicEnd
	if preprocessedTry.PanicBinding != "" {
		parts.panicBody = parts.panicBody[2:] // skip the generated declaration and blank use
	}
	parts.guard = &guard{
		layer:     layer,
		context:   &tryContext{isGuard: true, finally: layer, outer: parts.context},
		recovered: lowerer.fresh("recovered"),
		binding:   preprocessedTry.PanicBinding,
	}
}

// checkGuardedBody reports defer statements in a guarded try body. In the
// function literal, they would run at the end of the try body.
func (lowerer *lowerer) checkGuardedBody(body *ast.BlockStmt) {
	ast.Inspect(body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.DeferStmt:
			lowerer.errorf(lowerer.file, node.Pos(), "defer cannot be used directly in a try block with catch panic; it would run when the try block ends")
		}
		return true
	})
}

func (lowerer *lowerer) panicPrologue(parts *tryStatement) string {
	guard := parts.guard
	if guard.binding != "" && lowerer.usesObject(parts.panicBody, lowerer.findFirstDeclared(parts.panicBlock)) {
		return "\n" + guard.binding + " := " + guard.recovered + ";"
	}
	if isTerminating(parts.stmt.Body) {
		return "\n_ = " + guard.recovered + ";" // the jump to the block does not read it
	}
	return ""
}

// guardDecls declares the recovered value and the guard state.
func (lowerer *lowerer) guardDecls(parts *tryStatement) string {
	if parts.guard == nil {
		return ""
	}
	var decls strings.Builder
	fmt.Fprintf(&decls, "var %s any\n", parts.guard.recovered)
	lowerer.writeLayerDecls(&decls, parts.guard.layer)
	return decls.String()
}

// bodyOpen returns the code that opens the try body.
func (lowerer *lowerer) bodyOpen(parts *tryStatement) string {
	if parts.guard == nil {
		return "{"
	}
	return "{\nfunc() {\ndefer func() {\n" + parts.guard.recovered + " = recover()\n}();"
}

// bodyClose returns the code that closes the try body. With a guard, it
// resumes the pending exit or goes to the catch panic block.
func (lowerer *lowerer) bodyClose(parts *tryStatement) string {
	if parts.guard == nil {
		return "}"
	}
	var closing strings.Builder
	closing.WriteString("}()\n")
	writeExitCases(&closing, parts.guard.layer)
	// A try body that cannot complete ends only through an exit or a panic.
	if isTerminating(parts.stmt.Body) {
		fmt.Fprintf(&closing, "goto %s\n", parts.context.panicLabel())
	} else {
		fmt.Fprintf(&closing, "if %s != nil {\ngoto %s\n}\n", parts.guard.recovered, parts.context.panicLabel())
	}
	closing.WriteString("}")
	return closing.String()
}
