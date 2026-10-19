package transpile

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"strings"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// defaultCall is a call that omits parameters with default values. givenCount
// counts the parameters that the call passes.
type defaultCall struct {
	function   *defaultedFunc
	givenCount int
}

// defaultedFunc is a function declared in a .gol file with default values.
type defaultedFunc struct {
	object            *types.Func
	decl              *ast.FuncDecl
	firstDefaultIndex int
	values            []*defaultValue
}

// defaultValue is the default value of one parameter. A call that omits the
// parameter gets text, or a call to helper. The helper evaluates text in
// the scope of the declaration.
type defaultValue struct {
	paramName string
	text      string
	typeText  string
	line      int
	// objects holds the package-level and universe objects that text uses.
	// A call can copy text only where these names mean the same objects.
	objects []types.Object
	// copyable is false if text uses an import or has more than one line.
	copyable    bool
	needsHelper bool
	helperName  string
}

const notEnoughArguments = "not enough arguments in call to "

// resolveDefaults type-checks the default values and finds the calls that
// omit them. It runs after the last type-check pass.
func (transpiler *transpiler) resolveDefaults(typeInfo *types.Info) {
	transpiler.defaults = map[*types.Func]*defaultedFunc{}
	for _, file := range transpiler.files {
		if len(file.preprocessed.Defaults) == 0 {
			continue
		}
		for _, decl := range file.ast.Decls {
			funcDecl, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			defaults := file.findParamDefaults(funcDecl)
			if len(defaults) == 0 {
				continue
			}
			if function := transpiler.buildDefaultedFunc(file, funcDecl, defaults, typeInfo); function != nil {
				transpiler.defaults[function.object] = function
				file.defaultedFuncs = append(file.defaultedFuncs, function)
			}
		}
	}
	if len(transpiler.defaults) == 0 {
		return
	}
	for _, file := range transpiler.files {
		transpiler.findDefaultCalls(file, typeInfo)
	}
	transpiler.nameHelpers()
}

// findParamDefaults returns the defaults of the parameters of decl, in order.
func (file *file) findParamDefaults(decl *ast.FuncDecl) []preprocess.Default {
	var defaults []preprocess.Default
	for _, field := range decl.Type.Params.List {
		if len(field.Names) == 0 {
			continue
		}
		origOffset := file.preprocessed.OrigOffset(file.tokenFile.Offset(field.Names[0].Pos()))
		for _, paramDefault := range file.preprocessed.Defaults {
			if paramDefault.Param == origOffset {
				defaults = append(defaults, paramDefault)
			}
		}
	}
	return defaults
}

// buildDefaultedFunc checks the default values of decl. It returns nil if they
// have errors.
func (transpiler *transpiler) buildDefaultedFunc(file *file, decl *ast.FuncDecl, defaults []preprocess.Default, typeInfo *types.Info) *defaultedFunc {
	object, ok := typeInfo.Defs[decl.Name].(*types.Func)
	if !ok {
		return nil
	}
	signature := object.Signature()
	params := signature.Params()
	first := params.Len() - len(defaults)
	if signature.TypeParams().Len() > 0 || signature.RecvTypeParams().Len() > 0 {
		transpiler.diagnostics.Add(file.positionAt(defaults[0].Param), "default values are not supported in generic functions")
		return nil
	}
	function := &defaultedFunc{object: object, decl: decl, firstDefaultIndex: first}
	for defaultIndex, paramDefault := range defaults {
		param := params.At(first + defaultIndex)
		value := &defaultValue{
			paramName: param.Name(),
			typeText:  file.text(findParamType(decl, param.Pos())),
			line:      file.positionAt(paramDefault.Param).Line,
		}
		valueStart := paramDefault.ValueStart
		if paramDefault.Optional {
			value.text = zeroValue(param.Type(), value.typeText)
			valueStart = paramDefault.Param
		} else {
			value.text = string(file.original[paramDefault.ValueStart:paramDefault.ValueEnd])
		}
		if !transpiler.checkDefault(file, signature, value, valueStart) {
			return nil
		}
		function.values = append(function.values, value)
	}
	return function
}

// findParamType returns the type of the parameter field of decl at position.
func findParamType(decl *ast.FuncDecl, position token.Pos) ast.Expr {
	for _, field := range decl.Type.Params.List {
		for _, name := range field.Names {
			if name.Pos() == position {
				return field.Type
			}
		}
	}
	return nil
}

// checkDefault type-checks value as the result of a function at package
// level of file. So the value cannot use parameters. It records the objects
// that the value uses and reports errors at valueStart in the original
// source.
func (transpiler *transpiler) checkDefault(file *file, signature *types.Signature, value *defaultValue, valueStart int) bool {
	prefix := "func() " + value.typeText + " {\n\treturn "
	fileSet := token.NewFileSet()
	report := func(position token.Pos, message string) {
		offset := fileSet.Position(position).Offset - len(prefix)
		if offset < 0 || offset > len(value.text) {
			offset = 0
		}
		transpiler.diagnostics.Add(file.positionAt(valueStart+offset), message)
	}
	expr, err := parser.ParseExprFrom(fileSet, "", prefix+value.text+"\n}", 0)
	if err != nil {
		var errorList scanner.ErrorList
		if errors.As(err, &errorList) && len(errorList) > 0 {
			report(fileSet.File(1).Pos(errorList[0].Pos.Offset), errorList[0].Msg)
		} else {
			transpiler.diagnostics.Add(file.positionAt(valueStart), err.Error())
		}
		return false
	}
	usesInfo := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	if err := types.CheckExpr(fileSet, transpiler.checkedPackage, file.ast.Package, expr, usesInfo); err != nil {
		var typeError types.Error
		if !errors.As(err, &typeError) {
			transpiler.diagnostics.Add(file.positionAt(valueStart), err.Error())
			return false
		}
		report(typeError.Pos, formatDefaultMessage(typeError.Msg, signature))
		return false
	}
	value.copyable = !strings.Contains(value.text, "\n") && !strings.Contains(value.text, "//") && !strings.Contains(value.text, "/*")
	transpiler.recordDefaultUses(file, value, usesInfo, fileSet, len(prefix))
	return true
}

// recordDefaultUses records the objects and imports that value uses. Uses
// before prefixLength belong to the parameter type.
func (transpiler *transpiler) recordDefaultUses(file *file, value *defaultValue, usesInfo *types.Info, fileSet *token.FileSet, prefixLength int) {
	for ident, object := range usesInfo.Uses {
		if fileSet.Position(ident.Pos()).Offset < prefixLength {
			continue
		}
		switch {
		case isPkgName(object):
			// The helper keeps the import of the declaring file in use.
			value.copyable = false
			file.defaultImports[object.(*types.PkgName).Imported().Path()] = true
		case object.Parent() == transpiler.checkedPackage.Scope() || object.Parent() == types.Universe:
			value.objects = append(value.objects, object)
		}
	}
}

func isPkgName(object types.Object) bool {
	_, ok := object.(*types.PkgName)
	return ok
}

// formatDefaultMessage rewrites a type error in a default value. The value is
// checked as the result of a function, which the user does not see.
func formatDefaultMessage(message string, signature *types.Signature) string {
	if name, ok := strings.CutPrefix(message, "undefined: "); ok {
		for param := range signature.Params().Variables() {
			if param.Name() == name {
				return "a default value cannot use the parameter " + name
			}
		}
	}
	return strings.Replace(message, " in return statement", " in default value", 1)
}

// findDefaultCalls records the calls in file that omit parameters with
// default values. A call copies a value only where its names mean the same
// objects as at the declaration. Other calls need the helper.
func (transpiler *transpiler) findDefaultCalls(file *file, typeInfo *types.Info) {
	fileScope := typeInfo.Scopes[file.ast]
	ast.Inspect(file.ast, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || call.Ellipsis.IsValid() || hasTupleArg(call, typeInfo) {
			return true
		}
		function, receiverArgs := transpiler.findCalledDefaults(call.Fun, typeInfo)
		if function == nil {
			return true
		}
		givenCount := len(call.Args) - receiverArgs
		if givenCount < function.firstDefaultIndex || givenCount >= function.object.Signature().Params().Len() {
			return true
		}
		file.defaultCalls[call] = defaultCall{function: function, givenCount: givenCount}
		for _, value := range function.values[givenCount-function.firstDefaultIndex:] {
			value.needsHelper = value.needsHelper || !value.isCopyableAt(fileScope, call.Pos())
		}
		return true
	})
}

// isCopyableAt reports whether a call at position can use the text of value.
// Each name in the text must mean the same object there.
func (value *defaultValue) isCopyableAt(fileScope *types.Scope, position token.Pos) bool {
	if !value.copyable {
		return false
	}
	scope := fileScope.Innermost(position)
	for _, object := range value.objects {
		if _, found := scope.LookupParent(object.Name(), position); found != object {
			return false
		}
	}
	return true
}

func hasTupleArg(call *ast.CallExpr, typeInfo *types.Info) bool {
	if len(call.Args) != 1 {
		return false
	}
	_, isTuple := typeInfo.Types[call.Args[0]].Type.(*types.Tuple)
	return isTuple
}

// findCalledDefaults returns the function with defaults that fun names, and the
// number of arguments before its parameters. A method expression passes the
// receiver first.
func (transpiler *transpiler) findCalledDefaults(fun ast.Expr, typeInfo *types.Info) (*defaultedFunc, int) {
	var object types.Object
	receiverArgs := 0
	switch fun := ast.Unparen(fun).(type) {
	case *ast.Ident:
		object = typeInfo.Uses[fun]
	case *ast.SelectorExpr:
		object = typeInfo.Uses[fun.Sel]
		if typeInfo.Types[fun.X].IsType() {
			receiverArgs = 1
		}
	}
	function, ok := object.(*types.Func)
	if !ok {
		return nil, 0
	}
	return transpiler.defaults[function], receiverArgs
}

// nameHelpers names the helpers that calls or imports need. A helper name is
// not used anywhere in the package, so no local name can hide it.
func (transpiler *transpiler) nameHelpers() {
	taken := map[string]bool{}
	for _, name := range transpiler.checkedPackage.Scope().Names() {
		taken[name] = true
	}
	for _, file := range transpiler.files {
		for name := range file.identNames {
			taken[name] = true
		}
	}
	for _, file := range transpiler.files {
		for _, function := range file.defaultedFuncs {
			for _, value := range function.values {
				if value.copyable && !value.needsHelper {
					continue
				}
				value.helperName = pickFreshName(lowerFirst(getHelperBase(function.object)+upperFirst(value.paramName)+"Default"), taken)
			}
		}
	}
}

func getHelperBase(object *types.Func) string {
	receiver := object.Signature().Recv()
	if receiver == nil {
		return object.Name()
	}
	receiverType := receiver.Type()
	if pointer, ok := receiverType.(*types.Pointer); ok {
		receiverType = pointer.Elem()
	}
	if named, ok := receiverType.(*types.Named); ok {
		return named.Obj().Name() + upperFirst(object.Name())
	}
	return object.Name()
}

func pickFreshName(base string, taken map[string]bool) string {
	name := base
	for suffix := 2; taken[name]; suffix++ {
		name = fmt.Sprintf("%s%d", base, suffix)
	}
	taken[name] = true
	return name
}

func upperFirst(word string) string {
	if word == "" {
		return word
	}
	return strings.ToUpper(word[:1]) + word[1:]
}

func lowerFirst(word string) string {
	if word == "" {
		return word
	}
	return strings.ToLower(word[:1]) + word[1:]
}

// isCoveredByDefaults reports whether typeError is about a call that gosling
// completes with default values, or an import that only a default uses.
func (file *file) isCoveredByDefaults(typeError types.Error) bool {
	if strings.HasPrefix(typeError.Msg, notEnoughArguments) {
		for call := range file.defaultCalls {
			if call.Rparen == typeError.Pos {
				return true
			}
		}
		return false
	}
	for path := range file.defaultImports {
		if strings.HasPrefix(typeError.Msg, `"`+path+`" imported`) && strings.HasSuffix(typeError.Msg, "and not used") {
			return true
		}
	}
	return false
}

// lowerParamDefaults adds the omitted arguments to calls, and the helpers after
// their functions.
func (lowerer *lowerer) lowerParamDefaults() {
	for call, omitted := range lowerer.file.defaultCalls {
		lowerer.completeCall(call, omitted)
	}
	for _, function := range lowerer.file.defaultedFuncs {
		var helpers strings.Builder
		for _, value := range function.values {
			if value.helperName != "" {
				fmt.Fprintf(&helpers, "\n\nfunc %s() %s {\n\treturn %s\n}", value.helperName, value.typeText, value.text)
			}
		}
		if helpers.Len() > 0 {
			lowerer.insert(function.decl.End(), helpers.String(), function.values[0].line)
		}
	}
}

// reserveDefaultNames marks the names that completed calls add as used at
// each call. So temporaries around a call cannot hide them.
func (lowerer *lowerer) reserveDefaultNames() {
	for call, omitted := range lowerer.file.defaultCalls {
		for _, value := range omitted.function.values[omitted.givenCount-omitted.function.firstDefaultIndex:] {
			for _, object := range value.objects {
				lowerer.identPositions[object.Name()] = append(lowerer.identPositions[object.Name()], call.Rparen)
			}
			if value.helperName != "" {
				lowerer.identPositions[value.helperName] = append(lowerer.identPositions[value.helperName], call.Rparen)
			}
		}
	}
}

// completeCall adds the omitted arguments before the closing parenthesis of
// call. A trailing comma stays at the end.
func (lowerer *lowerer) completeCall(call *ast.CallExpr, omitted defaultCall) {
	var args []string
	fileScope := lowerer.typeInfo.Scopes[lowerer.file.ast]
	for _, value := range omitted.function.values[omitted.givenCount-omitted.function.firstDefaultIndex:] {
		if value.isCopyableAt(fileScope, call.Pos()) {
			args = append(args, value.text)
		} else {
			args = append(args, value.helperName+"()")
		}
	}
	text := strings.Join(args, ", ")
	if len(call.Args) > 0 {
		textAfterLastArg := lowerer.file.preprocessed.Src[lowerer.offset(call.Args[len(call.Args)-1].End()):lowerer.offset(call.Rparen)]
		if strings.Contains(string(textAfterLastArg), ",") {
			text += ","
		} else {
			text = ", " + text
		}
	}
	lowerer.insert(call.Rparen, text, lowerer.line(call.Rparen))
}
