package transpile

import (
	"fmt"
	"go/ast"
	"go/types"
	"strings"
)

// importArrayPackages imports the packages that the output of an array method
// call uses. It maps each package path to its qualifier, such as "slices.".
func (lowerer *lowerer) importArrayPackages(arrayCall *arrayCall) (map[string]string, bool) {
	var paths []string
	switch {
	case arrayCall.isMapCall():
	case arrayCall.method == "sort", arrayCall.method == "toSorted", arrayCall.method == "reverse",
		arrayCall.method == "toReversed", arrayCall.method == "slice", arrayCall.method == "flat":
		paths = []string{"slices"}
	case arrayCall.method == "join":
		paths = []string{"strings"}
		if !isStringLike(lowerer.elementType(arrayCall)) {
			paths = append(paths, "fmt")
		}
	}
	prefixes := map[string]string{}
	for _, path := range paths {
		name, ok := lowerer.importName(lowerer.file, path, path, false)
		if !ok {
			lowerer.errorf(lowerer.file, arrayCall.receiver.Pos(), "%s needs package %s, but the name %s is already used in this file", arrayCall.method, path, path)
			return nil, false
		}
		prefixes[path] = qualify(name)
	}
	return prefixes, true
}

func (lowerer *lowerer) elementType(arrayCall *arrayCall) types.Type {
	elementType, _, _ := elementTypes(lowerer.typeInfo.Types[arrayCall.receiver].Type)
	return elementType
}

func isStringLike(goType types.Type) bool {
	basic, ok := goType.Underlying().(*types.Basic)
	return ok && basic.Info()&types.IsString != 0
}

// findDirectForm returns the text for a call that becomes one function call,
// such as slices.Reverse(xs). It reports false for other calls. An array that
// cannot be sliced has no direct form.
func (lowerer *lowerer) findDirectForm(marker *ast.CallExpr, arrayCall *arrayCall, prefixes map[string]string) (callForm, bool) {
	_, isArray := lowerer.typeInfo.Types[arrayCall.receiver].Type.Underlying().(*types.Array)
	if arrayCall.isMapCall() || (isArray && !arrayCall.shouldSliceArray) {
		return callForm{}, false
	}
	stmt, isStmt := lowerer.parents[marker].(*ast.ExprStmt)
	isStmt = isStmt && stmt.X == marker
	slicesPrefix := prefixes["slices"]
	switch {
	case arrayCall.method == "sort" && isStmt && arrayCall.usesNaturalOrder:
		return callForm{slicesPrefix + "Sort(", "", ")"}, true
	case arrayCall.method == "sort" && isStmt:
		return callForm{slicesPrefix + "SortStableFunc(", ", ", ")"}, true
	case arrayCall.method == "reverse" && isStmt:
		return callForm{slicesPrefix + "Reverse(", "", ")"}, true
	case arrayCall.method == "flat":
		return callForm{slicesPrefix + "Concat(", "", "...)"}, true
	case arrayCall.method == "join" && types.Identical(lowerer.elementType(arrayCall), types.Typ[types.String]):
		return callForm{prefixes["strings"] + "Join(", ", ", ")"}, true
	}
	return callForm{}, false
}

// buildUtilityTail returns the code after the bindings for the methods that
// arrayLoop does not handle.
func (lowerer *lowerer) buildUtilityTail(arrayCall *arrayCall, typeText string, names arrayNames, argCount int, prefixes map[string]string) string {
	slicesPrefix := prefixes["slices"]
	sortCall := slicesPrefix + "SortStableFunc(%s, " + names.callback + ")"
	if arrayCall.usesNaturalOrder {
		sortCall = slicesPrefix + "Sort(%s)"
	}
	switch arrayCall.method {
	case "sort":
		return "\n" + fmt.Sprintf(sortCall, names.slice) + "\nreturn " + names.slice + "\n}()"
	case "toSorted":
		return fmt.Sprintf("\n%s := %sClone(%s)\n", names.out, slicesPrefix, names.slice) + fmt.Sprintf(sortCall, names.out) + "\nreturn " + names.out + "\n}()"
	case "reverse":
		return fmt.Sprintf("\n%sReverse(%[2]s)\nreturn %[2]s\n}()", slicesPrefix, names.slice)
	case "toReversed":
		return fmt.Sprintf("\n%[1]s := %[2]sClone(%[3]s)\n%[2]sReverse(%[1]s)\nreturn %[1]s\n}()", names.out, slicesPrefix, names.slice)
	case "slice":
		return buildSliceTail(slicesPrefix, names, argCount)
	case "join":
		return lowerer.buildJoinTail(arrayCall, names, prefixes)
	case "at":
		elementExpr := names.slice + "[" + names.index + "]"
		if arrayCall.returnsPointer {
			elementExpr = "&" + elementExpr
		}
		return fmt.Sprintf("\nif %[1]s < 0 {\n%[1]s += len(%[2]s)\n}\nif %[1]s < 0 || %[1]s >= len(%[2]s) {\nreturn nil\n}\nreturn %[3]s\n}()", names.index, names.slice, elementExpr)
	}
	return buildSearchTail(arrayCall, typeText, names)
}

// buildJoinTail writes each element with a strings.Builder. fmt formats
// elements that are not strings.
func (lowerer *lowerer) buildJoinTail(arrayCall *arrayCall, names arrayNames, prefixes map[string]string) string {
	writeElement := fmt.Sprintf("%s.WriteString(string(%s))", names.builder, names.element)
	if !isStringLike(lowerer.elementType(arrayCall)) {
		writeElement = fmt.Sprintf("%sFprint(&%s, %s)", prefixes["fmt"], names.builder, names.element)
	}
	return fmt.Sprintf("\nvar %[1]s %[2]sBuilder\nfor %[3]s, %[4]s := range %[5]s {\nif %[3]s > 0 {\n%[1]s.WriteString(%[6]s)\n}\n%[7]s\n}\nreturn %[1]s.String()\n}()",
		names.builder, prefixes["strings"], names.index, names.element, names.slice, names.separator, writeElement)
}

// buildSliceTail clamps the bounds as JavaScript does. A negative bound counts
// from the end.
func buildSliceTail(slicesPrefix string, names arrayNames, argCount int) string {
	replacer := strings.NewReplacer("xs", names.slice, "start", names.start, "end", names.end, "slices.", slicesPrefix)
	if argCount == 1 {
		return replacer.Replace("\nif start < 0 {\nstart += len(xs)\n}\nstart = min(max(start, 0), len(xs))\nreturn slices.Clone(xs[start:])\n}()")
	}
	return replacer.Replace("\nif start < 0 {\nstart += len(xs)\n}\nif end < 0 {\nend += len(xs)\n}\nstart = min(max(start, 0), len(xs))\nend = min(max(end, start), len(xs))\nreturn slices.Clone(xs[start:end])\n}()")
}

// buildSearchTail returns the loops of findLast, findLastIndex, flatMap, keys,
// and entries.
func buildSearchTail(arrayCall *arrayCall, typeText string, names arrayNames) string {
	element := names.slice + "[" + names.index + "]"
	callbackCall := fmt.Sprintf("%s(%s)", names.callback, element)
	if arrayCall.indexed {
		callbackCall = fmt.Sprintf("%s(%s, %s)", names.callback, element, names.index)
	}
	backwardLoop := fmt.Sprintf("\nfor %[1]s := len(%[2]s) - 1; %[1]s >= 0; %[1]s-- {\nif %[3]s {\nreturn %%s\n}\n}\nreturn %%s\n}()", names.index, names.slice, callbackCall)
	switch arrayCall.method {
	case "findLast":
		if arrayCall.returnsPointer {
			return fmt.Sprintf(backwardLoop, "&"+element, "nil")
		}
		return fmt.Sprintf(backwardLoop, element, "nil")
	case "findLastIndex":
		return fmt.Sprintf(backwardLoop, names.index, "-1")
	case "flatMap":
		flatLoop := fmt.Sprintf("for _, %s := range %s", names.element, names.slice)
		flatCall := fmt.Sprintf("%s(%s)", names.callback, names.element)
		if arrayCall.indexed {
			flatLoop = fmt.Sprintf("for %s, %s := range %s", names.index, names.element, names.slice)
			flatCall = fmt.Sprintf("%s(%s, %s)", names.callback, names.element, names.index)
		}
		return fmt.Sprintf("\nvar %[1]s %[2]s\n%[3]s {\n%[1]s = append(%[1]s, %[4]s...)\n}\nreturn %[1]s\n}()", names.out, typeText, flatLoop, flatCall)
	case "keys":
		return fmt.Sprintf("\n%[1]s := make([]int, len(%[2]s))\nfor %[3]s := range %[1]s {\n%[1]s[%[3]s] = %[3]s\n}\nreturn %[1]s\n}()", names.out, names.slice, names.index)
	case "entries":
		return fmt.Sprintf("\n%[1]s := make(%[2]s, len(%[3]s))\nfor %[4]s, %[5]s := range %[3]s {\n%[1]s[%[4]s].Index, %[1]s[%[4]s].Value = %[4]s, %[5]s\n}\nreturn %[1]s\n}()", names.out, typeText, names.slice, names.index, names.element)
	}
	return ""
}
