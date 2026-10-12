package transpile

import (
	"fmt"
	"go/ast"
	"go/types"
	"sort"
	"strings"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// arrayMethod describes a TypeScript-style method on slices.
type arrayMethod struct {
	callback       bool   // the first argument is a callback that takes the element
	extra          string // parameters of the marker after xs and the callback
	callbackParams string // callback parameters, before the optional index
	callbackResult string
	result         string
	typeParams     string
}

func (method arrayMethod) argCount() int {
	count := strings.Count(method.extra, ",")
	if method.callback {
		count++
	}
	return count
}

var arrayMethods = map[string]arrayMethod{
	"map":       {callback: true, callbackParams: "T", callbackResult: "R", result: "[]R", typeParams: "S ~[]T, T, R any"},
	"filter":    {callback: true, callbackParams: "T", callbackResult: "bool", result: "S", typeParams: "S ~[]T, T any"},
	"reduce":    {callback: true, extra: ", init R", callbackParams: "R, T", callbackResult: "R", result: "R", typeParams: "S ~[]T, T, R any"},
	"forEach":   {callback: true, callbackParams: "T", typeParams: "S ~[]T, T any"},
	"find":      {callback: true, callbackParams: "T", callbackResult: "bool", result: "T", typeParams: "S ~[]T, T any"},
	"findIndex": {callback: true, callbackParams: "T", callbackResult: "bool", result: "int", typeParams: "S ~[]T, T any"},
	"some":      {callback: true, callbackParams: "T", callbackResult: "bool", result: "bool", typeParams: "S ~[]T, T any"},
	"every":     {callback: true, callbackParams: "T", callbackResult: "bool", result: "bool", typeParams: "S ~[]T, T any"},
	"includes":  {extra: ", x T", result: "bool", typeParams: "S ~[]T, T comparable"},
	"indexOf":   {extra: ", x T", result: "int", typeParams: "S ~[]T, T comparable"},
}

// arrayCall is an array method call that a marker call replaces for type
// checking.
type arrayCall struct {
	method         string
	originalCall   *ast.CallExpr
	receiver       ast.Expr
	indexed        bool // the callback also takes the index
	returnsPointer bool // find returns a pointer into the slice
}

const arrayMarkerPrefix = "__goslingArr_"

// arrayMarkerDecls declares a generic marker for each method and variant, so
// a rewritten call type-checks with the correct result type.
func arrayMarkerDecls() string {
	names := make([]string, 0, len(arrayMethods))
	for name := range arrayMethods {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	for _, name := range names {
		method := arrayMethods[name]
		writeDecl := func(suffix, callbackParams, result string) {
			callbackParam := ""
			if method.callback {
				callbackParam = fmt.Sprintf(", f func(%s) %s", callbackParams, method.callbackResult)
			}
			body := "{ panic(0) }"
			if result == "" {
				body = "{}"
			}
			fmt.Fprintf(&builder, "func %s%s%s[%s](xs S%s%s) %s %s\n", arrayMarkerPrefix, name, suffix, method.typeParams, callbackParam, method.extra, result, body)
		}
		writeDecl("", method.callbackParams, method.result)
		if method.callback {
			writeDecl("I", method.callbackParams+", int", method.result)
		}
		if name == "find" {
			writeDecl("P", method.callbackParams, "*T")
			writeDecl("PI", method.callbackParams+", int", "*T")
		}
	}
	return builder.String()
}

// rewriteArrayCalls replaces calls like xs.filter(f) with marker calls if xs
// is a slice without such a method. It reports whether anything changed.
func (transpiler *transpiler) rewriteArrayCalls(file *file, info *types.Info) bool {
	changed := false
	ast.Inspect(file.ast, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		arrayCallInfo, ok := transpiler.asArrayCall(file, info, call)
		if !ok {
			return true
		}
		marker := &ast.CallExpr{
			Fun:    &ast.Ident{Name: arrayCallInfo.markerName(), NamePos: arrayCallInfo.receiver.Pos()},
			Lparen: call.Lparen,
			Args:   append([]ast.Expr{arrayCallInfo.receiver}, call.Args...),
			Rparen: call.Rparen,
		}
		file.arrayCalls[marker] = arrayCallInfo
		replaceNode(file.ast, call, marker)
		changed = true
		return false
	})
	return changed
}

// asArrayCall recognizes an array method call on a slice that has no real
// method of that name.
func (transpiler *transpiler) asArrayCall(file *file, info *types.Info, call *ast.CallExpr) (*arrayCall, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	name := selector.Sel.Name
	if name == preprocess.MapMarker {
		name = "map"
	}
	method, ok := arrayMethods[name]
	if !ok {
		return nil, false
	}
	receiverType := info.Types[selector.X].Type
	slice, ok := underlyingSlice(receiverType)
	if !ok {
		return nil, false
	}
	if object, _, _ := types.LookupFieldOrMethod(receiverType, true, transpiler.checkedPackage, name); object != nil {
		return nil, false // a real method wins
	}
	arrayCallInfo := &arrayCall{method: name, originalCall: call, receiver: selector.X}
	if !method.callback {
		return arrayCallInfo, true
	}
	if len(call.Args) == 0 {
		return nil, false
	}
	count, known := transpiler.callbackParams(file, info, call.Args[0])
	if !known {
		return nil, false
	}
	arrayCallInfo.indexed = count == strings.Count(method.callbackParams, ",")+2
	arrayCallInfo.returnsPointer = name == "find" && !nilable(slice.Elem())
	return arrayCallInfo, true
}

// markerName picks the marker variant. P is for a find that returns a pointer
// into the slice. I is for a callback that also takes the index.
func (call *arrayCall) markerName() string {
	name := arrayMarkerPrefix + call.method
	if call.returnsPointer {
		name += "P"
	}
	if call.indexed {
		name += "I"
	}
	return name
}

// callbackParams returns how many parameters a callback argument takes.
func (transpiler *transpiler) callbackParams(file *file, info *types.Info, arg ast.Expr) (int, bool) {
	for _, arrowFunc := range file.arrows {
		if arrowFunc.node() == arg {
			return arrowFunc.paramCount(), true
		}
	}
	if signature, ok := info.Types[arg].Type.(*types.Signature); ok {
		return signature.Params().Len(), true
	}
	return 0, false
}

// callbackContext returns the parameter and result types that an array
// method gives to its callback.
func (transpiler *transpiler) callbackContext(info *types.Info, call *ast.CallExpr, arrayCall *arrayCall, arrowFunc *arrow) (funcContext, bool) {
	slice, ok := underlyingSlice(info.Types[call.Args[0]].Type)
	if !ok || len(call.Args) < 2 || call.Args[1] != arrowFunc.node() {
		return funcContext{}, false
	}
	elementType := slice.Elem()
	context := funcContext{params: []types.Type{elementType}, resultsKnown: true}
	switch arrayCall.method {
	case "map":
		context.resultsKnown = false
	case "reduce":
		if len(call.Args) < 3 {
			return funcContext{}, false
		}
		accumulator := types.Default(info.Types[call.Args[2]].Type)
		if accumulator == nil {
			return funcContext{}, false
		}
		context.params = []types.Type{accumulator, elementType}
		context.results = []types.Type{accumulator}
	case "forEach":
	default:
		context.results = []types.Type{types.Typ[types.Bool]}
	}
	if arrayCall.indexed {
		context.params = append(context.params, types.Typ[types.Int])
	}
	return context, true
}

// lowerArrays changes marker calls into loops inside immediately called
// function literals. It binds the receiver and arguments first:
//
//	func() []R { xs, f := recv, callback; ...loop...; return out }()
func (lowerer *lowerer) lowerArrays() {
	for marker, arrayCall := range lowerer.file.arrayCalls {
		lowerer.lowerArrayCall(marker, arrayCall)
	}
}

func (lowerer *lowerer) lowerArrayCall(marker *ast.CallExpr, arrayCall *arrayCall) {
	// The marker holds the current arguments. The original call can still hold
	// stand-ins for arrows resolved after it.
	receiver, args := arrayCall.receiver, marker.Args[1:]
	method := arrayMethods[arrayCall.method]
	typeText, ok := lowerer.checkArrayCall(marker, arrayCall, method)
	if !ok {
		return
	}
	bindNames, separator := lowerer.arrayBindings(arrayCall, method)
	open := "func() " + typeText + " {\n" + bindNames + " := "
	if method.result == "" {
		open = "{\n" + bindNames + " := "
	}
	line, depth := lowerer.line(receiver.Pos()), lowerer.depth(marker)
	lowerer.insertOrdered(receiver.Pos(), open, line, depth)
	lowerer.replace(receiver.End(), args[0].Pos(), separator, line)
	tail := arrayLoop(arrayCall, typeText)
	if !method.callback {
		tail = ")" + tail
	}
	lowerer.replace(args[len(args)-1].End(), arrayCall.originalCall.Rparen+1, tail, line)
}

// checkArrayCall reports misuse of an array method. It returns the result
// type as the file writes it, or "" for forEach.
func (lowerer *lowerer) checkArrayCall(marker *ast.CallExpr, arrayCall *arrayCall, method arrayMethod) (string, bool) {
	receiver := arrayCall.receiver
	stmt, isStmt := lowerer.parents[marker].(*ast.ExprStmt)
	if method.result == "" && !(isStmt && stmt.X == marker) {
		lowerer.errorf(lowerer.file, receiver.Pos(), "%s can only be used as a statement", arrayCall.method)
		return "", false
	}
	if wantArgs := method.argCount(); len(marker.Args)-1 != wantArgs {
		lowerer.errorf(lowerer.file, receiver.Pos(), "%s expects %d argument(s)", arrayCall.method, wantArgs)
		return "", false
	}
	if method.result == "" {
		return "", true
	}
	typeText, ok := lowerer.typeName(lowerer.info.Types[marker].Type)
	if !ok {
		lowerer.typeNameTakenError(receiver.Pos(), arrayCall.method, typeText)
	}
	return typeText, ok
}

// arrayBindings returns the names for the receiver and arguments, and the
// text between the receiver and the first argument.
func (lowerer *lowerer) arrayBindings(arrayCall *arrayCall, method arrayMethod) (string, string) {
	switch {
	case arrayCall.method == "reduce":
		return "xs, f, acc", ", "
	case !method.callback:
		slice, _ := underlyingSlice(lowerer.info.Types[arrayCall.receiver].Type)
		elementType, _ := lowerer.typeName(slice.Elem())
		return "xs, v", ", " + elementType + "("
	}
	return "xs, f", ", "
}

// arrayLoop returns the code after the bindings for each method.
func arrayLoop(arrayCall *arrayCall, typeText string) string {
	loop, call := "for _, x := range xs", "f(x)"
	if arrayCall.indexed {
		loop, call = "for i, x := range xs", "f(x, i)"
	}
	switch arrayCall.method {
	case "map":
		return fmt.Sprintf("\nout := make(%s, 0, len(xs))\n%s {\nout = append(out, %s)\n}\nreturn out\n}()", typeText, loop, call)
	case "filter":
		return fmt.Sprintf("\nvar out %s\n%s {\nif %s {\nout = append(out, x)\n}\n}\nreturn out\n}()", typeText, loop, call)
	case "reduce":
		call = strings.Replace(call, "f(", "f(acc, ", 1)
		return fmt.Sprintf("\n%s {\nacc = %s\n}\nreturn acc\n}()", loop, call)
	case "forEach":
		return fmt.Sprintf("\n%s {\n%s\n}\n}", loop, call)
	case "find":
		if arrayCall.returnsPointer {
			return fmt.Sprintf("\nfor i, x := range xs {\nif %s {\nreturn &xs[i]\n}\n}\nreturn nil\n}()", call)
		}
		return fmt.Sprintf("\n%s {\nif %s {\nreturn x\n}\n}\nreturn nil\n}()", loop, call)
	case "findIndex":
		return fmt.Sprintf("\nfor i, x := range xs {\nif %s {\nreturn i\n}\n}\nreturn -1\n}()", call)
	case "some":
		return fmt.Sprintf("\n%s {\nif %s {\nreturn true\n}\n}\nreturn false\n}()", loop, call)
	case "every":
		return fmt.Sprintf("\n%s {\nif !%s {\nreturn false\n}\n}\nreturn true\n}()", loop, call)
	case "includes":
		return "\nfor _, x := range xs {\nif x == v {\nreturn true\n}\n}\nreturn false\n}()"
	case "indexOf":
		return "\nfor i, x := range xs {\nif x == v {\nreturn i\n}\n}\nreturn -1\n}()"
	}
	return ""
}
