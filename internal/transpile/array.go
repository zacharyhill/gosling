package transpile

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strconv"
	"strings"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// arrayMethod describes a TypeScript-style method on slices, arrays, or maps.
type arrayMethod struct {
	hasCallback    bool   // the first argument is a callback that takes the element
	extraParams    string // parameters of the marker after xs and the callback
	callbackParams string // callback parameters, before the optional index
	callbackResult string
	result         string
	// pointerResult is the result of the variant for elements that cannot be
	// nil. This variant returns a pointer, or nil.
	pointerResult      string
	typeParams         string
	loopBuiltins       []string
	optionalArgCount   int  // the number of trailing arguments that can be left out
	allowsNaturalOrder bool // without a callback, the method uses the natural order
}

func (method arrayMethod) argCount() int {
	count := strings.Count(method.extraParams, ",")
	if method.hasCallback {
		count++
	}
	return count
}

var arrayMethods = map[string]arrayMethod{
	"map":       {hasCallback: true, callbackParams: "T", callbackResult: "R", result: "[]R", typeParams: "S ~[]T, T, R any", loopBuiltins: []string{"make", "len", "append"}},
	"filter":    {hasCallback: true, callbackParams: "T", callbackResult: "bool", result: "S", typeParams: "S ~[]T, T any", loopBuiltins: []string{"append"}},
	"reduce":    {hasCallback: true, extraParams: ", init R", callbackParams: "R, T", callbackResult: "R", result: "R", typeParams: "S ~[]T, T, R any"},
	"forEach":   {hasCallback: true, callbackParams: "T", typeParams: "S ~[]T, T any"},
	"find":      {hasCallback: true, callbackParams: "T", callbackResult: "bool", result: "T", pointerResult: "*T", typeParams: "S ~[]T, T any", loopBuiltins: []string{"nil"}},
	"findIndex": {hasCallback: true, callbackParams: "T", callbackResult: "bool", result: "int", typeParams: "S ~[]T, T any"},
	"some":      {hasCallback: true, callbackParams: "T", callbackResult: "bool", result: "bool", typeParams: "S ~[]T, T any", loopBuiltins: []string{"true", "false"}},
	"every":     {hasCallback: true, callbackParams: "T", callbackResult: "bool", result: "bool", typeParams: "S ~[]T, T any", loopBuiltins: []string{"true", "false"}},
	"includes":  {extraParams: ", x T", result: "bool", typeParams: "S ~[]T, T comparable", loopBuiltins: []string{"true", "false"}},
	"indexOf":   {extraParams: ", x T", result: "int", typeParams: "S ~[]T, T comparable"},

	"sort":          {hasCallback: true, allowsNaturalOrder: true, callbackParams: "T, T", callbackResult: "int", result: "S", typeParams: "S ~[]T, T any"},
	"toSorted":      {hasCallback: true, allowsNaturalOrder: true, callbackParams: "T, T", callbackResult: "int", result: "S", typeParams: "S ~[]T, T any"},
	"reverse":       {result: "S", typeParams: "S ~[]T, T any"},
	"toReversed":    {result: "S", typeParams: "S ~[]T, T any"},
	"slice":         {extraParams: ", start int, end ...int", optionalArgCount: 1, result: "S", typeParams: "S ~[]T, T any", loopBuiltins: []string{"len", "min", "max"}},
	"join":          {extraParams: ", sep string", result: "string", typeParams: "S ~[]T, T any", loopBuiltins: []string{"string"}},
	"flatMap":       {hasCallback: true, callbackParams: "T", callbackResult: "[]R", result: "[]R", typeParams: "S ~[]T, T, R any", loopBuiltins: []string{"append"}},
	"flat":          {result: "E", typeParams: "S ~[]E, E ~[]T, T any"},
	"at":            {extraParams: ", i int", result: "T", pointerResult: "*T", typeParams: "S ~[]T, T any", loopBuiltins: []string{"len", "nil"}},
	"findLast":      {hasCallback: true, callbackParams: "T", callbackResult: "bool", result: "T", pointerResult: "*T", typeParams: "S ~[]T, T any", loopBuiltins: []string{"len", "nil"}},
	"findLastIndex": {hasCallback: true, callbackParams: "T", callbackResult: "bool", result: "int", typeParams: "S ~[]T, T any", loopBuiltins: []string{"len"}},
	"keys":          {result: "[]int", typeParams: "S ~[]T, T any", loopBuiltins: []string{"make", "len", "int"}},
	"entries":       {result: "[]struct{ Index int; Value T }", typeParams: "S ~[]T, T any", loopBuiltins: []string{"make", "len"}},
}

// orderedConstraint matches the types that slices.Sort accepts. The marker
// file cannot import cmp.
const orderedConstraint = "~int | ~int8 | ~int16 | ~int32 | ~int64 | ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr | ~float32 | ~float64 | ~string"

const mapTypeParams = "M ~map[K]V, K comparable, V any"

var mapMethods = map[string]arrayMethod{
	"has":     {extraParams: ", k K", result: "bool", typeParams: mapTypeParams},
	"get":     {extraParams: ", k K", result: "V", pointerResult: "*V", typeParams: mapTypeParams, loopBuiltins: []string{"nil"}},
	"keys":    {result: "[]K", typeParams: mapTypeParams, loopBuiltins: []string{"make", "len", "append"}},
	"values":  {result: "[]V", typeParams: mapTypeParams, loopBuiltins: []string{"make", "len", "append"}},
	"entries": {result: "[]struct{ Key K; Value V }", typeParams: mapTypeParams, loopBuiltins: []string{"make", "len"}},
	"delete":  {extraParams: ", k K", typeParams: mapTypeParams, loopBuiltins: []string{"delete"}},
	"forEach": {hasCallback: true, callbackParams: "V", typeParams: mapTypeParams},
}

// methodSet holds the methods of one kind of receiver.
type methodSet struct {
	markerPrefix      string
	receiverTypeParam string
	callbackIndexType string
	methods           map[string]arrayMethod
}

var (
	sliceMethodSet = methodSet{markerPrefix: arrayMarkerPrefix, receiverTypeParam: "S", callbackIndexType: "int", methods: arrayMethods}
	mapMethodSet   = methodSet{markerPrefix: mapMarkerPrefix, receiverTypeParam: "M", callbackIndexType: "K", methods: mapMethods}
)

// arrayCall is an array or map method call that a marker call replaces for
// type checking.
type arrayCall struct {
	method         string
	set            *methodSet
	originalCall   *ast.CallExpr
	receiver       ast.Expr
	indexed        bool // the callback also takes the index or key
	returnsPointer bool // find, at, or get returns a pointer
	// usesNaturalOrder is true for sort or toSorted without a comparator.
	usesNaturalOrder bool
	// isFirstElementInit is true for a reduce without an initial value.
	isFirstElementInit bool
	// shouldSliceArray is true for an addressable array. The output slices it,
	// so the loop does not copy it and find points into it.
	shouldSliceArray bool
}

func (call *arrayCall) definition() arrayMethod { return call.set.methods[call.method] }

func (call *arrayCall) isMapCall() bool { return call.set == &mapMethodSet }

const (
	arrayMarkerPrefix = "__goslingArr_"
	mapMarkerPrefix   = "__goslingKV_"
	// arrayRefMarker lets an array receiver type-check as a slice, also if
	// the array is not addressable.
	arrayRefMarker = "__goslingArrayRef"
)

func isMethodMarker(name string) bool {
	return strings.HasPrefix(name, arrayMarkerPrefix) || strings.HasPrefix(name, mapMarkerPrefix)
}

// arrayMarkerDecls declares a generic marker for each method and variant, so
// a rewritten call type-checks with the correct result type.
func arrayMarkerDecls() string {
	var builder strings.Builder
	builder.WriteString("func " + arrayRefMarker + "[A any](a A) *A { return &a }\n")
	sliceMethodSet.writeMarkerDecls(&builder)
	mapMethodSet.writeMarkerDecls(&builder)
	// A reduce without an initial value accumulates the element type.
	const reduceFirstDecl = "func %sreduce%s[S ~[]T, T any](xs S, f func(T, T%s) T) T { panic(0) }\n"
	fmt.Fprintf(&builder, reduceFirstDecl, arrayMarkerPrefix, "F", "")
	fmt.Fprintf(&builder, reduceFirstDecl, arrayMarkerPrefix, "FI", ", int")
	return builder.String()
}

func (set *methodSet) writeMarkerDecls(builder *strings.Builder) {
	names := make([]string, 0, len(set.methods))
	for name := range set.methods {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		method := set.methods[name]
		writeDecl := func(suffix, typeParams, callbackParams, result string) {
			callbackParam := ""
			if callbackParams != "" {
				callbackParam = fmt.Sprintf(", f func(%s) %s", callbackParams, method.callbackResult)
			}
			body := "{ panic(0) }"
			if result == "" {
				body = "{}"
			}
			fmt.Fprintf(builder, "func %s%s%s[%s](xs %s%s%s) %s %s\n", set.markerPrefix, name, suffix, typeParams, set.receiverTypeParam, callbackParam, method.extraParams, result, body)
		}
		callbackParams := ""
		if method.hasCallback {
			callbackParams = method.callbackParams
		}
		indexedParams := callbackParams + ", " + set.callbackIndexType
		writeDecl("", method.typeParams, callbackParams, method.result)
		if method.hasCallback {
			writeDecl("I", method.typeParams, indexedParams, method.result)
		}
		if method.allowsNaturalOrder {
			writeDecl("N", "S ~[]T, T "+orderedConstraint, "", method.result)
		}
		if method.pointerResult == "" {
			continue
		}
		writeDecl("P", method.typeParams, callbackParams, method.pointerResult)
		if method.hasCallback {
			writeDecl("PI", method.typeParams, indexedParams, method.pointerResult)
		}
	}
}

// rewriteArrayCalls replaces calls like xs.filter(f) with marker calls if xs
// is a slice, array, or map without such a method. It reports whether
// anything changed.
func (transpiler *transpiler) rewriteArrayCalls(file *file, typeInfo *types.Info) bool {
	changed := false
	ast.Inspect(file.ast, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		methodCall, ok := transpiler.asArrayCall(file, typeInfo, call)
		if !ok {
			return true
		}
		marker := &ast.CallExpr{
			Fun:    &ast.Ident{Name: methodCall.markerName(), NamePos: methodCall.receiver.Pos()},
			Lparen: call.Lparen,
			Args:   append([]ast.Expr{buildReceiverArg(typeInfo, methodCall.receiver)}, call.Args...),
			Rparen: call.Rparen,
		}
		file.arrayCalls[marker] = methodCall
		replaceNode(file.ast, call, marker)
		changed = true
		return false
	})
	return changed
}

// buildReceiverArg returns the first marker argument. An array becomes
// __goslingArrayRef(a)[:], a slice of the same elements.
func buildReceiverArg(typeInfo *types.Info, receiver ast.Expr) ast.Expr {
	if _, isArray := typeInfo.Types[receiver].Type.Underlying().(*types.Array); !isArray {
		return receiver
	}
	return &ast.SliceExpr{X: &ast.CallExpr{
		Fun:    &ast.Ident{Name: arrayRefMarker, NamePos: receiver.Pos()},
		Args:   []ast.Expr{receiver},
		Rparen: receiver.End(),
	}}
}

// elementTypes returns the element type of a slice, array, or map, and the
// type of its index or key.
func elementTypes(goType types.Type) (types.Type, types.Type, bool) {
	if goType == nil {
		return nil, nil, false
	}
	switch underlying := goType.Underlying().(type) {
	case *types.Slice:
		return underlying.Elem(), types.Typ[types.Int], true
	case *types.Array:
		return underlying.Elem(), types.Typ[types.Int], true
	case *types.Map:
		return underlying.Elem(), underlying.Key(), true
	}
	return nil, nil, false
}

func methodSetFor(goType types.Type) *methodSet {
	if _, isMap := goType.Underlying().(*types.Map); isMap {
		return &mapMethodSet
	}
	return &sliceMethodSet
}

// asArrayCall recognizes an array or map method call on a receiver that has
// no real method of that name.
func (transpiler *transpiler) asArrayCall(file *file, typeInfo *types.Info, call *ast.CallExpr) (*arrayCall, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	name := selector.Sel.Name
	if name == preprocess.MapMarker {
		name = "map"
	}
	receiverType := typeInfo.Types[selector.X].Type
	elementType, _, ok := elementTypes(receiverType)
	if !ok {
		return nil, false
	}
	set := methodSetFor(receiverType)
	method, ok := set.methods[name]
	if !ok {
		return nil, false
	}
	if object, _, _ := types.LookupFieldOrMethod(receiverType, true, transpiler.checkedPackage, name); object != nil {
		return nil, false // a real method wins
	}
	methodCall := &arrayCall{method: name, set: set, originalCall: call, receiver: selector.X}
	methodCall.returnsPointer = method.pointerResult != "" && !isComparableToNil(elementType)
	if _, isArray := receiverType.Underlying().(*types.Array); isArray {
		methodCall.shouldSliceArray = typeInfo.Types[selector.X].Addressable()
	}
	if !method.hasCallback {
		return methodCall, true
	}
	if len(call.Args) == 0 {
		methodCall.usesNaturalOrder = method.allowsNaturalOrder
		return methodCall, method.allowsNaturalOrder
	}
	count, isKnown := transpiler.callbackParams(file, typeInfo, call.Args[0])
	if !isKnown {
		return nil, false
	}
	methodCall.indexed = count == strings.Count(method.callbackParams, ",")+2
	methodCall.isFirstElementInit = set == &sliceMethodSet && name == "reduce" && len(call.Args) == 1
	return methodCall, true
}

// markerName picks the marker variant. N is for a sort in the natural order.
// F is for a reduce that starts with the first element. P is for a call that
// returns a pointer. I is for a callback that also takes the index or key.
func (call *arrayCall) markerName() string {
	name := call.set.markerPrefix + call.method
	if call.isFirstElementInit {
		name += "F"
	}
	if call.usesNaturalOrder {
		name += "N"
	}
	if call.returnsPointer {
		name += "P"
	}
	if call.indexed {
		name += "I"
	}
	return name
}

// callbackParams returns how many parameters a callback argument takes.
func (transpiler *transpiler) callbackParams(file *file, typeInfo *types.Info, arg ast.Expr) (int, bool) {
	argIdent, _ := ast.Unparen(arg).(*ast.Ident)
	argObject := typeInfo.Uses[argIdent]
	for _, arrowFunc := range file.arrows {
		if arrowFunc.node() == arg {
			return arrowFunc.paramCount(), true
		}
		isUntypedArrowVariable := arrowFunc.nilStandIn != nil && argObject != nil && argObject == arrowFunc.declaredVariable(typeInfo)
		if isUntypedArrowVariable {
			return arrowFunc.paramCount(), true
		}
	}
	if signature, ok := typeInfo.Types[arg].Type.(*types.Signature); ok {
		return signature.Params().Len(), true
	}
	return 0, false
}

// callbackContext returns the parameter and result types that an array
// method gives to its callback.
func (transpiler *transpiler) callbackContext(typeInfo *types.Info, call *ast.CallExpr, arrayCall *arrayCall, callback ast.Expr) (funcContext, bool) {
	elementType, indexType, ok := elementTypes(typeInfo.Types[call.Args[0]].Type)
	if !ok || len(call.Args) < 2 || call.Args[1] != callback {
		return funcContext{}, false
	}
	context := funcContext{params: []types.Type{elementType}, resultsKnown: true}
	switch arrayCall.method {
	case "map", "flatMap":
		context.resultsKnown = false
	case "sort", "toSorted":
		context.params = []types.Type{elementType, elementType}
		context.results = []types.Type{types.Typ[types.Int]}
	case "reduce":
		if arrayCall.isFirstElementInit {
			context.params = []types.Type{elementType, elementType}
			context.results = []types.Type{elementType}
			break
		}
		if len(call.Args) < 3 {
			return funcContext{}, false
		}
		accumulator := types.Default(typeInfo.Types[call.Args[2]].Type)
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
		context.params = append(context.params, indexType)
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
	receiver, arguments := arrayCall.receiver, marker.Args[1:]
	method := arrayCall.definition()
	typeText, ok := lowerer.checkArrayCall(marker, arrayCall, method)
	if !ok || !lowerer.checkBuiltins(receiver.Pos(), arrayCall.method, method.loopBuiltins...) {
		return
	}
	if arrayCall.isFirstElementInit && !lowerer.checkBuiltins(receiver.Pos(), arrayCall.method, "len", "panic") {
		return
	}
	prefixes, ok := lowerer.importArrayPackages(arrayCall)
	if !ok {
		return
	}
	names := pickArrayNames(typeText)
	line, depth := lowerer.line(receiver.Pos()), lowerer.depth(marker)
	if lowerer.lowerMapAccess(arrayCall, arguments, typeText, names, line, depth) {
		return
	}
	if form, isDirect := lowerer.findDirectForm(marker, arrayCall, prefixes); isDirect {
		lowerer.wrapArrayCall(arrayCall, arguments, form, line, depth)
		return
	}
	bindNames, separator := lowerer.arrayBindings(arrayCall, method, names, len(arguments))
	open := "func() " + typeText + " {\n" + bindNames + " := "
	if method.result == "" {
		lowerer.dropParens(marker)
		open = "{\n" + bindNames + " := "
	}
	tail := arrayLoop(arrayCall, typeText, names)
	if tail == "" {
		tail = lowerer.buildUtilityTail(arrayCall, typeText, names, len(arguments), prefixes)
	}
	if !arrayCall.isMapCall() && (arrayCall.method == "includes" || arrayCall.method == "indexOf") {
		tail = ")" + tail
	}
	lowerer.wrapArrayCall(arrayCall, arguments, callForm{open, separator, tail}, line, depth)
}

// callForm holds the text that replaces the parts of an array method call
// around its receiver and arguments.
type callForm struct{ open, separator, closing string }

func (lowerer *lowerer) wrapArrayCall(arrayCall *arrayCall, arguments []ast.Expr, form callForm, line, depth int) {
	receiver, end := arrayCall.receiver, arrayCall.originalCall.Rparen+1
	receiverEnd := ""
	if arrayCall.shouldSliceArray {
		receiverEnd = "[:]"
	}
	lowerer.insertOrdered(receiver.Pos(), form.open, line, depth)
	if len(arguments) == 0 {
		lowerer.replace(receiver.End(), end, receiverEnd+form.closing, line)
		return
	}
	lowerer.replace(receiver.End(), arguments[0].Pos(), receiverEnd+form.separator, line)
	lowerer.replace(arguments[len(arguments)-1].End(), end, form.closing, line)
}

// lowerMapAccess lowers has, get, and delete on a map to index expressions
// or the delete builtin. It reports false for other calls.
func (lowerer *lowerer) lowerMapAccess(arrayCall *arrayCall, arguments []ast.Expr, typeText string, names arrayNames, line, depth int) bool {
	if !arrayCall.isMapCall() || arrayCall.definition().hasCallback || len(arguments) != 1 {
		return false
	}
	open, separator, closing := "", "[", "]"
	switch {
	case arrayCall.method == "has":
		open, closing = "func() bool {\n_, "+names.found+" := ", "]\nreturn "+names.found+"\n}()"
	case arrayCall.method == "delete":
		open, separator, closing = "delete(", ", ", ")"
	case arrayCall.returnsPointer:
		open = fmt.Sprintf("func() %s {\nif %s, %s := ", typeText, names.value, names.found)
		closing = fmt.Sprintf("]; %[2]s {\nreturn &%[1]s\n}\nreturn nil\n}()", names.value, names.found)
	}
	receiver := arrayCall.receiver
	if open != "" {
		lowerer.insertOrdered(receiver.Pos(), open, line, depth)
	}
	lowerer.replace(receiver.End(), arguments[0].Pos(), separator, line)
	lowerer.replace(arguments[0].End(), arrayCall.originalCall.Rparen+1, closing, line)
	return true
}

// checkArrayCall reports misuse of an array method. It returns the result
// type as the file writes it, or "" for forEach.
func (lowerer *lowerer) checkArrayCall(marker *ast.CallExpr, arrayCall *arrayCall, method arrayMethod) (string, bool) {
	receiver := arrayCall.receiver
	if method.result == "" && !lowerer.isStatement(marker) {
		lowerer.errorf(lowerer.file, receiver.Pos(), "%s can only be used as a statement", arrayCall.method)
		return "", false
	}
	if !lowerer.checkArgCount(marker, arrayCall, method) {
		return "", false
	}
	if method.result == "" {
		return "", true
	}
	resultType := lowerer.typeInfo.Types[marker].Type
	typeText, ok := lowerer.typeName(resultType)
	if !ok {
		lowerer.typeNameTakenError(receiver.Pos(), arrayCall.method, typeText)
		return typeText, false
	}
	if lowerer.reportHiddenType(resultType, typeText, receiver.Pos()) {
		return typeText, false
	}
	if elementType, _, isCollection := elementTypes(lowerer.typeInfo.Types[receiver].Type); isCollection && !method.hasCallback {
		elementText, _ := lowerer.typeName(elementType)
		return typeText, !lowerer.reportHiddenType(elementType, elementText, receiver.Pos())
	}
	return typeText, true
}

// checkArgCount reports a call with the wrong number of arguments.
func (lowerer *lowerer) checkArgCount(marker *ast.CallExpr, arrayCall *arrayCall, method arrayMethod) bool {
	wantArgs, gotArgs := method.argCount(), len(marker.Args)-1
	if arrayCall.usesNaturalOrder {
		wantArgs = 0
	}
	if arrayCall.isFirstElementInit {
		wantArgs--
	}
	minArgs := wantArgs - method.optionalArgCount
	if gotArgs <= wantArgs && gotArgs >= minArgs {
		return true
	}
	receiver := arrayCall.receiver
	if method.optionalArgCount > 0 {
		lowerer.errorf(lowerer.file, receiver.Pos(), "%s expects %d to %d argument(s)", arrayCall.method, minArgs, wantArgs)
	} else {
		lowerer.errorf(lowerer.file, receiver.Pos(), "%s expects %d argument(s)", arrayCall.method, wantArgs)
	}
	return false
}

// arrayNames holds the names of the locals in a lowered array method call.
type arrayNames struct {
	slice, mapName, callback, accumulator, value, element, index, key, found, out string
	separator, start, end, builder                                                string
}

// pickArrayNames picks the local names for an array method call. The result
// type text comes after the bindings, so no name may occur in it.
func pickArrayNames(typeText string) arrayNames {
	reserved := collectIdentifiers(typeText)
	pickName := func(base string) string {
		name := base
		for suffix := 2; reserved[name]; suffix++ {
			name = base + strconv.Itoa(suffix)
		}
		reserved[name] = true
		return name
	}
	return arrayNames{
		slice:       pickName("xs"),
		mapName:     pickName("m"),
		callback:    pickName("f"),
		accumulator: pickName("acc"),
		value:       pickName("v"),
		element:     pickName("x"),
		index:       pickName("i"),
		key:         pickName("k"),
		found:       pickName("ok"),
		out:         pickName("out"),
		separator:   pickName("sep"),
		start:       pickName("start"),
		end:         pickName("end"),
		builder:     pickName("b"),
	}
}

// collectIdentifiers returns the set of identifiers in source.
func collectIdentifiers(source string) map[string]bool {
	identifiers := map[string]bool{}
	sourceScanner, _ := newScanner([]byte(source), 0)
	for {
		_, kind, identText := sourceScanner.Scan()
		if kind == token.EOF {
			return identifiers
		}
		if kind == token.IDENT {
			identifiers[identText] = true
		}
	}
}

// arrayBindings returns the names for the receiver and arguments, and the
// text between the receiver and the first argument.
func (lowerer *lowerer) arrayBindings(arrayCall *arrayCall, method arrayMethod, names arrayNames, argCount int) (string, string) {
	switch {
	case arrayCall.isMapCall() && argCount == 0:
		return names.mapName, ""
	case argCount == 0:
		return names.slice, ""
	case arrayCall.isMapCall() && method.hasCallback:
		return names.mapName + ", " + names.callback, ", "
	case arrayCall.isMapCall():
		return names.mapName, ""
	case arrayCall.method == "reduce" && !arrayCall.isFirstElementInit:
		return names.slice + ", " + names.callback + ", " + names.accumulator, ", "
	case arrayCall.method == "slice" && argCount == 2:
		return names.slice + ", " + names.start + ", " + names.end, ", "
	case arrayCall.method == "slice":
		return names.slice + ", " + names.start, ", "
	case arrayCall.method == "join":
		return names.slice + ", " + names.separator, ", "
	case arrayCall.method == "at":
		return names.slice + ", " + names.index, ", "
	case !method.hasCallback:
		elementType, _, _ := elementTypes(lowerer.typeInfo.Types[arrayCall.receiver].Type)
		elementText, _ := lowerer.typeName(elementType)
		return names.slice + ", " + names.value, ", " + elementText + "("
	}
	return names.slice + ", " + names.callback, ", "
}

// arrayLoop returns the code after the bindings for each method.
func arrayLoop(arrayCall *arrayCall, typeText string, names arrayNames) string {
	if arrayCall.isMapCall() {
		return mapLoop(arrayCall, typeText, names)
	}
	loop := fmt.Sprintf("for _, %s := range %s", names.element, names.slice)
	call := fmt.Sprintf("%s(%s)", names.callback, names.element)
	indexedLoop := fmt.Sprintf("for %s, %s := range %s", names.index, names.element, names.slice)
	if arrayCall.indexed {
		loop, call = indexedLoop, fmt.Sprintf("%s(%s, %s)", names.callback, names.element, names.index)
	}
	switch arrayCall.method {
	case "map":
		return fmt.Sprintf("\n%[1]s := make(%[2]s, 0, len(%[3]s))\n%[4]s {\n%[1]s = append(%[1]s, %[5]s)\n}\nreturn %[1]s\n}()", names.out, typeText, names.slice, loop, call)
	case "filter":
		return fmt.Sprintf("\nvar %[1]s %[2]s\n%[3]s {\nif %[4]s {\n%[1]s = append(%[1]s, %[5]s)\n}\n}\nreturn %[1]s\n}()", names.out, typeText, loop, call, names.element)
	case "reduce":
		if arrayCall.isFirstElementInit {
			return buildReduceFirstLoop(arrayCall, names)
		}
		call = strings.Replace(call, names.callback+"(", names.callback+"("+names.accumulator+", ", 1)
		return fmt.Sprintf("\n%[1]s {\n%[2]s = %[3]s\n}\nreturn %[2]s\n}()", loop, names.accumulator, call)
	case "forEach":
		return fmt.Sprintf("\n%s {\n%s\n}\n}", loop, call)
	case "find":
		if arrayCall.returnsPointer {
			return fmt.Sprintf("\n%[1]s {\nif %[2]s {\nreturn &%[3]s[%[4]s]\n}\n}\nreturn nil\n}()", indexedLoop, call, names.slice, names.index)
		}
		return fmt.Sprintf("\n%s {\nif %s {\nreturn %s\n}\n}\nreturn nil\n}()", loop, call, names.element)
	case "findIndex":
		return fmt.Sprintf("\n%s {\nif %s {\nreturn %s\n}\n}\nreturn -1\n}()", indexedLoop, call, names.index)
	case "some":
		return fmt.Sprintf("\n%s {\nif %s {\nreturn true\n}\n}\nreturn false\n}()", loop, call)
	case "every":
		return fmt.Sprintf("\n%s {\nif !%s {\nreturn false\n}\n}\nreturn true\n}()", loop, call)
	case "includes":
		return fmt.Sprintf("\nfor _, %[1]s := range %[2]s {\nif %[1]s == %[3]s {\nreturn true\n}\n}\nreturn false\n}()", names.element, names.slice, names.value)
	case "indexOf":
		return fmt.Sprintf("\n%s {\nif %s == %s {\nreturn %s\n}\n}\nreturn -1\n}()", indexedLoop, names.element, names.value, names.index)
	}
	return ""
}

// mapLoop returns the code after the bindings for each map method.
func mapLoop(arrayCall *arrayCall, typeText string, names arrayNames) string {
	collectTemplate := "\n%[1]s := make(%[2]s, 0, len(%[3]s))\nfor %[4]s := range %[3]s {\n%[1]s = append(%[1]s, %[5]s)\n}\nreturn %[1]s\n}()"
	switch arrayCall.method {
	case "keys":
		return fmt.Sprintf(collectTemplate, names.out, typeText, names.mapName, names.key, names.key)
	case "values":
		return fmt.Sprintf(collectTemplate, names.out, typeText, names.mapName, "_, "+names.value, names.value)
	case "entries":
		// Index assignment does not repeat the struct type.
		return fmt.Sprintf("\n%[1]s := make(%[2]s, len(%[3]s))\n%[4]s := 0\nfor %[5]s, %[6]s := range %[3]s {\n%[1]s[%[4]s].Key, %[1]s[%[4]s].Value = %[5]s, %[6]s\n%[4]s++\n}\nreturn %[1]s\n}()",
			names.out, typeText, names.mapName, names.index, names.key, names.value)
	case "forEach":
		if arrayCall.indexed {
			return fmt.Sprintf("\nfor %[1]s, %[2]s := range %[3]s {\n%[4]s(%[2]s, %[1]s)\n}\n}", names.key, names.value, names.mapName, names.callback)
		}
		return fmt.Sprintf("\nfor _, %[1]s := range %[2]s {\n%[3]s(%[1]s)\n}\n}", names.value, names.mapName, names.callback)
	}
	return ""
}

// emptyReduceMessage is the panic message of a reduce without an initial
// value on an empty slice. JavaScript throws a TypeError in this case.
const emptyReduceMessage = "reduce of empty slice with no initial value"

// buildReduceFirstLoop starts the accumulator with the first element.
func buildReduceFirstLoop(arrayCall *arrayCall, names arrayNames) string {
	loop := fmt.Sprintf("for _, %[1]s := range %[2]s[1:] {\n%[3]s = %[4]s(%[3]s, %[1]s)\n}", names.element, names.slice, names.accumulator, names.callback)
	if arrayCall.indexed {
		loop = fmt.Sprintf("for %[1]s := 1; %[1]s < len(%[2]s); %[1]s++ {\n%[3]s = %[4]s(%[3]s, %[2]s[%[1]s], %[1]s)\n}", names.index, names.slice, names.accumulator, names.callback)
	}
	return fmt.Sprintf("\nif len(%[1]s) == 0 {\npanic(%[2]q)\n}\n%[3]s := %[1]s[0]\n%[4]s\nreturn %[3]s\n}()", names.slice, emptyReduceMessage, names.accumulator, loop)
}
