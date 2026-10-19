package transpile

import (
	"fmt"
	"go/ast"
	"go/types"
	"sort"
	"strings"
)

// stringMethod describes a TypeScript-style method on strings.
type stringMethod struct {
	params string // parameters of the marker after the receiver
	result string
	// function is the strings function that the call becomes. An empty
	// function means that the call becomes a function literal.
	function string
	// extraArgs follow the arguments in the strings call.
	extraArgs string
	// trimsSpace adds unicode.IsSpace after the arguments.
	trimsSpace bool
}

var stringMethods = map[string]stringMethod{
	"includes":    {params: ", x string", result: "bool", function: "Contains"},
	"startsWith":  {params: ", x string", result: "bool", function: "HasPrefix"},
	"endsWith":    {params: ", x string", result: "bool", function: "HasSuffix"},
	"split":       {params: ", separator string", result: "[]string", function: "Split"},
	"trim":        {result: "string", function: "TrimSpace"},
	"trimStart":   {result: "string", function: "TrimLeftFunc", trimsSpace: true},
	"trimEnd":     {result: "string", function: "TrimRightFunc", trimsSpace: true},
	"toUpperCase": {result: "string", function: "ToUpper"},
	"toLowerCase": {result: "string", function: "ToLower"},
	"replace":     {params: ", old, replacement string", result: "string", function: "Replace", extraArgs: ", 1"},
	"replaceAll":  {params: ", old, replacement string", result: "string", function: "ReplaceAll"},
	"indexOf":     {params: ", x string", result: "int", function: "Index"},
	"lastIndexOf": {params: ", x string", result: "int", function: "LastIndex"},
	"repeat":      {params: ", count int", result: "string", function: "Repeat"},
	"slice":       {params: ", start int, end ...int", result: "string"},
	"padStart":    {params: ", width int, pad ...string", result: "string"},
	"padEnd":      {params: ", width int, pad ...string", result: "string"},
	"at":          {params: ", index int", result: "string"},
}

// stringCall is a string method call that a marker call replaces for type
// checking.
type stringCall struct {
	method   string
	receiver ast.Expr
}

const stringMarkerPrefix = "__goslingStr_"

// stringMarkerDecls declares a marker for each string method. The receiver
// is generic, so named string types also have the methods.
func stringMarkerDecls() string {
	names := make([]string, 0, len(stringMethods))
	for name := range stringMethods {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	for _, name := range names {
		method := stringMethods[name]
		fmt.Fprintf(&builder, "func %s%s[S ~string](s S%s) %s { panic(0) }\n", stringMarkerPrefix, name, method.params, method.result)
	}
	return builder.String()
}

// rewriteStringCalls replaces calls like s.includes(x) with marker calls if
// s is a string without such a method. It reports whether anything changed.
func (transpiler *transpiler) rewriteStringCalls(file *file, typeInfo *types.Info) bool {
	changed := false
	ast.Inspect(file.ast, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !transpiler.hasStringMethod(typeInfo.Types[selector.X].Type, selector.Sel.Name) {
			return true
		}
		marker := &ast.CallExpr{
			Fun:    &ast.Ident{Name: stringMarkerPrefix + selector.Sel.Name, NamePos: selector.X.Pos()},
			Lparen: call.Lparen,
			Args:   append([]ast.Expr{selector.X}, call.Args...),
			Rparen: call.Rparen,
		}
		file.stringCalls[marker] = &stringCall{method: selector.Sel.Name, receiver: selector.X}
		replaceNode(file.ast, call, marker)
		changed = true
		return false
	})
	return changed
}

// hasStringMethod reports whether receiverType is a string type that gets
// the string method name. A real method of the type wins.
func (transpiler *transpiler) hasStringMethod(receiverType types.Type, name string) bool {
	if _, ok := stringMethods[name]; !ok || receiverType == nil {
		return false
	}
	basic, ok := receiverType.Underlying().(*types.Basic)
	if !ok || basic.Info()&types.IsString == 0 {
		return false
	}
	object, _, _ := types.LookupFieldOrMethod(receiverType, true, transpiler.checkedPackage, name)
	return object == nil
}

// lowerStrings changes the marker calls into strings calls or function
// literals.
func (lowerer *lowerer) lowerStrings() {
	for marker, call := range lowerer.file.stringCalls {
		lowerer.lowerStringCall(marker, call)
	}
}

func (lowerer *lowerer) lowerStringCall(marker *ast.CallExpr, call *stringCall) {
	method := stringMethods[call.method]
	arguments := marker.Args[1:]
	if strings.Contains(method.params, "...") && len(arguments) > 2 {
		lowerer.errorf(lowerer.file, call.receiver.Pos(), "%s expects 1 or 2 arguments", call.method)
		return
	}
	if !lowerer.checkBuiltins(call.receiver.Pos(), call.method, lowerer.findStringBuiltins(call)...) {
		return
	}
	stringsName := ""
	if method.function != "" || strings.HasPrefix(call.method, "pad") {
		var ok bool
		if stringsName, ok = lowerer.importQualifier(call, "strings"); !ok {
			return
		}
	}
	if method.function == "" {
		lowerer.lowerStringLiteral(marker, call, stringsName)
		return
	}
	extraArgs := method.extraArgs
	if method.trimsSpace {
		unicodeName, ok := lowerer.importQualifier(call, "unicode")
		if !ok {
			return
		}
		extraArgs = ", " + unicodeName + "IsSpace"
	}
	receiverOpen, receiverClose := lowerer.receiverConversion(call)
	line := lowerer.line(call.receiver.Pos())
	lowerer.insertOrdered(call.receiver.Pos(), stringsName+method.function+"("+receiverOpen, line, lowerer.depth(marker))
	if len(arguments) == 0 {
		lowerer.replace(call.receiver.End(), marker.Rparen+1, receiverClose+extraArgs+")", line)
		return
	}
	lowerer.replace(call.receiver.End(), arguments[0].Pos(), receiverClose+", ", line)
	lowerer.replace(arguments[len(arguments)-1].End(), marker.Rparen+1, extraArgs+")", line)
}

// lowerStringLiteral writes a call as a function literal that binds the
// receiver and the arguments.
func (lowerer *lowerer) lowerStringLiteral(marker *ast.CallExpr, call *stringCall, stringsName string) {
	arguments := marker.Args[1:]
	receiverOpen, receiverClose := lowerer.receiverConversion(call)
	line := lowerer.line(call.receiver.Pos())
	open := "func() string {\n" + buildLiteralBindings(call.method, len(arguments)) + " := " + receiverOpen
	lowerer.insertOrdered(call.receiver.Pos(), open, line, lowerer.depth(marker))
	lowerer.replace(call.receiver.End(), arguments[0].Pos(), receiverClose+", ", line)
	body := buildLiteralBody(call.method, len(arguments), stringsName)
	lowerer.replace(arguments[len(arguments)-1].End(), marker.Rparen+1, "\n"+body+"\n}()", line)
}

// receiverConversion returns the text around a receiver of a named string
// type, which the strings functions do not accept.
func (lowerer *lowerer) receiverConversion(call *stringCall) (string, string) {
	if types.Identical(types.Default(lowerer.typeInfo.Types[call.receiver].Type), types.Typ[types.String]) {
		return "", ""
	}
	return "string(", ")"
}

// findStringBuiltins returns the builtins that the lowered call uses.
func (lowerer *lowerer) findStringBuiltins(call *stringCall) []string {
	var builtins []string
	if stringMethods[call.method].function == "" {
		builtins = append(builtins, "len", "max", "min")
	}
	if opening, _ := lowerer.receiverConversion(call); opening != "" {
		builtins = append(builtins, "string")
	}
	return builtins
}

// importQualifier returns the qualifier for a package that the lowered
// call uses. It adds the import if the file does not have one.
func (lowerer *lowerer) importQualifier(call *stringCall, path string) (string, bool) {
	name, ok := lowerer.importName(lowerer.file, path, path, false)
	if !ok {
		lowerer.errorf(lowerer.file, call.receiver.Pos(), "%s needs package %s, but the name %s is already used in this file", call.method, path, path)
		return "", false
	}
	return qualify(name), true
}

// buildLiteralBindings returns the names that a function literal binds to the
// receiver and the arguments.
func buildLiteralBindings(method string, argCount int) string {
	switch {
	case method == "at":
		return "s, index"
	case method == "slice" && argCount == 2:
		return "s, start, end"
	case method == "slice":
		return "s, start"
	case argCount == 2:
		return "s, width, pad"
	}
	return "s, width"
}

// buildLiteralBody returns the code after the bindings. slice works as in
// JavaScript: a negative index counts from the end. Indexes are bytes.
func buildLiteralBody(method string, argCount int, stringsName string) string {
	switch method {
	case "at":
		return "if index < 0 {\nindex += len(s)\n}\nreturn s[index : index+1]"
	case "slice":
		body := "if start < 0 {\nstart = max(len(s)+start, 0)\n}\nif end < 0 {\nend = max(len(s)+end, 0)\n}\n" +
			"start, end = min(start, len(s)), min(end, len(s))\nif start >= end {\nreturn \"\"\n}\nreturn s[start:end]"
		if argCount == 1 {
			body = "end := len(s)\n" + body
		}
		return body
	}
	body := "if len(s) >= width || pad == \"\" {\nreturn s\n}\n" +
		"fill := " + stringsName + "Repeat(pad, (width-len(s)+len(pad)-1)/len(pad))[:width-len(s)]\n"
	if argCount == 1 {
		body = "pad := \" \"\n" + body
	}
	if method == "padStart" {
		return body + "return fill + s"
	}
	return body + "return s + fill"
}
