package transpile

import (
	"fmt"
	"go/ast"
	"go/types"
	"strconv"
	"strings"
)

// fusibleMethods can end a fused chain. Only filter and map can come before
// another call in the chain.
var fusibleMethods = map[string]bool{
	"map": true, "filter": true, "reduce": true, "forEach": true, "find": true,
	"findIndex": true, "some": true, "every": true, "includes": true, "indexOf": true,
}

// findFusedChains groups chained array calls, such as xs.filter(f).map(g), that
// lower to one loop. It maps the outermost call of each group of two or more
// calls to the group, innermost first. It also returns the other calls of
// the groups.
func (lowerer *lowerer) findFusedChains() (map[*ast.CallExpr][]*ast.CallExpr, map[*ast.CallExpr]bool) {
	outerOf := map[*ast.CallExpr]*ast.CallExpr{}
	for marker, arrayCall := range lowerer.file.arrayCalls {
		if inner, ok := lowerer.getInnerArrayCall(arrayCall); ok {
			outerOf[inner] = marker
		}
	}
	groups, isInner := map[*ast.CallExpr][]*ast.CallExpr{}, map[*ast.CallExpr]bool{}
	addGroup := func(group []*ast.CallExpr) {
		if len(group) < 2 {
			return
		}
		groups[group[len(group)-1]] = group
		for _, marker := range group[:len(group)-1] {
			isInner[marker] = true
		}
	}
	for marker, arrayCall := range lowerer.file.arrayCalls {
		if _, hasInner := lowerer.getInnerArrayCall(arrayCall); hasInner {
			continue
		}
		group := []*ast.CallExpr{marker}
		for outer := outerOf[marker]; outer != nil; outer = outerOf[outer] {
			if !lowerer.canFuse(group, outer) {
				addGroup(group)
				group = nil
			}
			group = append(group, outer)
		}
		addGroup(group)
	}
	return groups, isInner
}

// getInnerArrayCall returns the array call that is the receiver of arrayCall.
func (lowerer *lowerer) getInnerArrayCall(arrayCall *arrayCall) (*ast.CallExpr, bool) {
	inner, ok := arrayCall.receiver.(*ast.CallExpr)
	return inner, ok && lowerer.file.arrayCalls[inner] != nil
}

// canFuse reports whether outer can join the loop of group without a change
// in results. Its arguments must not depend on when they are evaluated. A
// callback that takes an index cannot follow a filter, because the index
// would count the elements before the filter.
func (lowerer *lowerer) canFuse(group []*ast.CallExpr, outer *ast.CallExpr) bool {
	last, outerCall := lowerer.file.arrayCalls[group[len(group)-1]], lowerer.file.arrayCalls[outer]
	if last.isMapCall() || outerCall.isMapCall() || outerCall.isFirstElementInit {
		return false
	}
	if (last.method != "filter" && last.method != "map") || !fusibleMethods[outerCall.method] {
		return false
	}
	for _, arg := range outer.Args[1:] {
		if !lowerer.isStableExpr(arg) {
			return false
		}
	}
	if !usesIndex(outerCall) {
		return true
	}
	for _, marker := range group {
		if lowerer.file.arrayCalls[marker].method == "filter" {
			return false
		}
	}
	return true
}

func usesIndex(arrayCall *arrayCall) bool {
	return arrayCall.indexed || arrayCall.method == "findIndex" || arrayCall.method == "indexOf"
}

// isStableExpr reports whether expr gives the same value before and after the
// earlier callbacks of a chain run. Function literals, constants, and
// declared functions qualify.
func (lowerer *lowerer) isStableExpr(expr ast.Expr) bool {
	if lowerer.typeInfo.Types[expr].Value != nil {
		return true
	}
	switch expr := expr.(type) {
	case *ast.FuncLit:
		return true
	case *ast.ParenExpr:
		return lowerer.isStableExpr(expr.X)
	case *ast.Ident:
		_, isFunc := lowerer.typeInfo.Uses[expr].(*types.Func)
		return isFunc
	case *ast.SelectorExpr:
		packageIdent, ok := expr.X.(*ast.Ident)
		if !ok {
			return false
		}
		_, isPackage := lowerer.typeInfo.Uses[packageIdent].(*types.PkgName)
		_, isFunc := lowerer.typeInfo.Uses[expr.Sel].(*types.Func)
		return isPackage && isFunc
	}
	return false
}

// lowerFusedChain lowers a group of chained calls to one loop. The binding
// statement takes the receiver and each argument in source order:
//
//	func() []R { xs, f1, f2 := recv, callback1, callback2; ...loop...; return out }()
func (lowerer *lowerer) lowerFusedChain(group []*ast.CallExpr) {
	steps, typeText, ok := lowerer.checkFusedSteps(group)
	if !ok {
		return
	}
	names := pickArrayNames(typeText)
	terminal, outermost := steps[len(steps)-1], group[len(group)-1]
	receiver := steps[0].receiver
	line := lowerer.line(receiver.Pos())
	closureHeader := "func() " + typeText + " {\n" + buildFusedBindings(steps, names) + " := "
	if terminal.method == "forEach" {
		lowerer.dropParens(outermost)
		closureHeader = "{\n" + buildFusedBindings(steps, names) + " := "
	}
	lowerer.insertOrdered(receiver.Pos(), closureHeader, line, lowerer.depth(outermost))
	previousEnd, receiverEnd := receiver.End(), ""
	if steps[0].shouldSliceArray {
		receiverEnd = "[:]"
	}
	for _, marker := range group {
		arguments := marker.Args[1:]
		lowerer.replace(previousEnd, arguments[0].Pos(), receiverEnd+lowerer.buildFusedSeparator(lowerer.file.arrayCalls[marker]), line)
		previousEnd, receiverEnd = arguments[len(arguments)-1].End(), ""
	}
	tail := buildFusedLoop(steps, typeText, names)
	if !terminal.definition().hasCallback {
		tail = ")" + tail
	}
	lowerer.replace(previousEnd, terminal.originalCall.Rparen+1, tail, line)
}

// checkFusedSteps checks each call of a chain. It returns the calls and the
// result type of the chain as the file writes it.
func (lowerer *lowerer) checkFusedSteps(group []*ast.CallExpr) ([]*arrayCall, string, bool) {
	steps := make([]*arrayCall, len(group))
	typeText := ""
	for stepIndex, marker := range group {
		step := lowerer.file.arrayCalls[marker]
		steps[stepIndex] = step
		method := step.definition()
		stepType, ok := lowerer.checkArrayCall(marker, step, method)
		if !ok || !lowerer.checkBuiltins(step.receiver.Pos(), step.method, method.loopBuiltins...) {
			return nil, "", false
		}
		typeText = stepType
	}
	return steps, typeText, true
}

// buildFusedSeparator returns the text before the first argument of a call. A
// value that the loop compares with gets the element type.
func (lowerer *lowerer) buildFusedSeparator(arrayCall *arrayCall) string {
	if arrayCall.definition().hasCallback {
		return ", "
	}
	elementType, _, _ := elementTypes(lowerer.typeInfo.Types[arrayCall.receiver].Type)
	elementText, _ := lowerer.typeName(elementType)
	return ", " + elementText + "("
}

// buildFusedBindings returns the names that the binding statement declares.
func buildFusedBindings(steps []*arrayCall, names arrayNames) string {
	bindings := []string{names.slice}
	for stepIndex, step := range steps {
		if step.definition().hasCallback {
			bindings = append(bindings, names.callback+strconv.Itoa(stepIndex+1))
		}
	}
	switch steps[len(steps)-1].method {
	case "reduce":
		bindings = append(bindings, names.accumulator)
	case "includes", "indexOf":
		bindings = append(bindings, names.value)
	}
	return strings.Join(bindings, ", ")
}

// buildFusedLoop returns the code after the bindings. Each filter skips the rest
// of the iteration. Each map stores its result in a new variable.
func buildFusedLoop(steps []*arrayCall, typeText string, names arrayNames) string {
	var body strings.Builder
	current, indexName, hasFilter, mapCount := names.element, "_", false, 0
	for stepIndex, step := range steps {
		hasFilter = hasFilter || step.method == "filter"
		if usesIndex(step) {
			indexName = names.index
		}
		if stepIndex == len(steps)-1 {
			break
		}
		call := buildFusedCall(step, stepIndex, current, names)
		if step.method == "filter" {
			fmt.Fprintf(&body, "if !%s {\ncontinue\n}\n", call)
			continue
		}
		mapCount++
		mappedValue := names.element + strconv.Itoa(mapCount+1)
		fmt.Fprintf(&body, "%s := %s\n", mappedValue, call)
		current = mappedValue
	}
	terminal := steps[len(steps)-1]
	before, inLoop, after := buildFusedTerminal(terminal, buildFusedCall(terminal, len(steps)-1, current, names), current, typeText, hasFilter, names)
	return fmt.Sprintf("\n%sfor %s, %s := range %s {\n%s%s}\n%s", before, indexName, names.element, names.slice, body.String(), inLoop, after)
}

// buildFusedCall returns the call of the callback of a step.
func buildFusedCall(step *arrayCall, stepIndex int, current string, names arrayNames) string {
	arguments := current
	if step.method == "reduce" {
		arguments = names.accumulator + ", " + current
	}
	if step.indexed {
		arguments += ", " + names.index
	}
	return fmt.Sprintf("%s%d(%s)", names.callback, stepIndex+1, arguments)
}

// buildFusedTerminal returns the code of the last call of a chain: before the
// loop, at the end of the loop body, and after the loop.
func buildFusedTerminal(terminal *arrayCall, call, current, typeText string, hasFilter bool, names arrayNames) (string, string, string) {
	out, accumulator, index, value := names.out, names.accumulator, names.index, names.value
	switch terminal.method {
	case "map", "filter":
		before := fmt.Sprintf("%s := make(%s, 0, len(%s))\n", out, typeText, names.slice)
		if hasFilter {
			before = fmt.Sprintf("var %s %s\n", out, typeText)
		}
		if terminal.method == "map" {
			return before, fmt.Sprintf("%[1]s = append(%[1]s, %[2]s)\n", out, call), "return " + out + "\n}()"
		}
		return before, fmt.Sprintf("if %[2]s {\n%[1]s = append(%[1]s, %[3]s)\n}\n", out, call, current), "return " + out + "\n}()"
	case "reduce":
		return "", fmt.Sprintf("%s = %s\n", accumulator, call), "return " + accumulator + "\n}()"
	case "forEach":
		return "", call + "\n", "}"
	case "find":
		// The earlier calls make a new slice, so the pointer goes to a copy.
		found := current
		if terminal.returnsPointer {
			found = "&" + current
		}
		return "", fmt.Sprintf("if %s {\nreturn %s\n}\n", call, found), "return nil\n}()"
	case "findIndex":
		return "", fmt.Sprintf("if %s {\nreturn %s\n}\n", call, index), "return -1\n}()"
	case "some":
		return "", fmt.Sprintf("if %s {\nreturn true\n}\n", call), "return false\n}()"
	case "every":
		return "", fmt.Sprintf("if !%s {\nreturn false\n}\n", call), "return true\n}()"
	case "includes":
		return "", fmt.Sprintf("if %s == %s {\nreturn true\n}\n", current, value), "return false\n}()"
	case "indexOf":
		return "", fmt.Sprintf("if %s == %s {\nreturn %s\n}\n", current, value, index), "return -1\n}()"
	}
	return "", "", ""
}
