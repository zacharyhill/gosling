package transpile

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strings"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// arrow is one arrow function. Several type-check passes fill in its
// placeholder types, from the source, the surrounding context, or the type
// of its body.
type arrow struct {
	funcLit *ast.FuncLit
	// untyped holds the placeholder type of each parameter without a written
	// type. While any are unknown, nilStandIn replaces the literal in the AST.
	// So the literal does not affect inference of the surrounding call.
	untyped           []*ast.Ident
	nilStandIn        *ast.Ident
	params            []types.Type
	resultPlaceholder *ast.Ident
	bodyReturn        *ast.ReturnStmt // the return of an expression body
	resolved          bool            // result type is isKnown
	results           []types.Type    // empty for no result
	usedAsStmt        bool            // expression body used as a statement
	// hasHiddenType is true when a local declaration hides the name of a type
	// that the arrow needs. gosling already reported the error.
	hasHiddenType bool
	isVoidBlock   bool // block body without a value return; only the context gives results
}

func (arrow *arrow) node() ast.Expr {
	if arrow.nilStandIn != nil {
		return arrow.nilStandIn
	}
	return arrow.funcLit
}

func (arrow *arrow) paramCount() int { return arrow.funcLit.Type.Params.NumFields() }

// findArrows records the arrows in file and prepares them for the first pass.
func (file *file) findArrows() {
	ast.Inspect(file.ast, func(node ast.Node) bool {
		funcLit, ok := node.(*ast.FuncLit)
		if !ok || funcLit.Type.Results == nil || len(funcLit.Type.Results.List) != 1 {
			return true
		}
		resultIdent, ok := funcLit.Type.Results.List[0].Type.(*ast.Ident)
		if !ok || resultIdent.Name != preprocess.ResultMarker {
			return true
		}
		arrowFunc := &arrow{funcLit: funcLit, resultPlaceholder: resultIdent}
		for _, field := range funcLit.Type.Params.List {
			if paramIdent, ok := field.Type.(*ast.Ident); ok && paramIdent.Name == preprocess.UntypedMarker {
				arrowFunc.untyped = append(arrowFunc.untyped, paramIdent)
			}
		}
		if len(funcLit.Body.List) == 1 {
			arrowFunc.bodyReturn, _ = funcLit.Body.List[0].(*ast.ReturnStmt)
		}
		if arrowFunc.bodyReturn == nil && !returnsValue(funcLit.Body) {
			funcLit.Type.Results = nil
			arrowFunc.isVoidBlock = true
		}
		file.arrows = append(file.arrows, arrowFunc)
		return true
	})
	for _, arrowFunc := range file.arrows {
		if len(arrowFunc.untyped) > 0 {
			arrowFunc.nilStandIn = &ast.Ident{Name: "nil", NamePos: arrowFunc.funcLit.Pos()}
			replaceNode(file.ast, arrowFunc.funcLit, arrowFunc.nilStandIn)
		}
	}
}

func returnsValue(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			found = found || len(node.Results) > 0
		}
		return !found
	})
	return found
}

// funcContext holds what the surrounding code expects of a function value.
type funcContext struct {
	params  []types.Type
	results []types.Type
	// resultsKnown is false if the results must come from the body, as for
	// the callback of map.
	resultsKnown bool
}

// fixArrows fills in the arrow types that the last pass found. It reports
// whether anything changed.
func (transpiler *transpiler) fixArrows(file *file, typeInfo *types.Info) bool {
	changed := false
	parentOf := parents(file.ast)
	for _, arrowFunc := range file.arrows {
		if (arrowFunc.nilStandIn == nil && arrowFunc.resolved) || arrowFunc.hasHiddenType {
			continue
		}
		context, ok := transpiler.arrowContext(file, typeInfo, parentOf, arrowFunc)
		ok = ok && !slices.ContainsFunc(context.params, hasPlaceholder) && !slices.ContainsFunc(context.results, hasPlaceholder)
		if arrowFunc.nilStandIn != nil {
			if !ok || len(context.params) != arrowFunc.paramCount() {
				continue
			}
			if !transpiler.setParams(file, arrowFunc, context.params) {
				continue
			}
			changed = true
		}
		if arrowFunc.resolved {
			continue
		}
		switch {
		case ok && context.resultsKnown:
			changed = transpiler.setResults(file, arrowFunc, context.results) || changed
		case arrowFunc.nilStandIn == nil:
			if results, ok := file.bodyResults(typeInfo, arrowFunc); ok {
				changed = transpiler.setResults(file, arrowFunc, results) || changed
			}
		}
	}
	return changed
}

func (transpiler *transpiler) setParams(file *file, arrowFunc *arrow, params []types.Type) bool {
	fields := arrowFunc.funcLit.Type.Params.List
	typeExprs := make([]ast.Expr, len(params))
	for paramIndex, param := range params {
		paramExpr, ok := transpiler.arrowTypeExpr(file, arrowFunc, param, fields[paramIndex].Type.Pos())
		if !ok {
			return false
		}
		typeExprs[paramIndex] = paramExpr
	}
	for paramIndex, field := range fields {
		field.Type = typeExprs[paramIndex]
	}
	arrowFunc.params = params
	replaceNode(file.ast, arrowFunc.nilStandIn, arrowFunc.funcLit)
	arrowFunc.nilStandIn = nil
	return true
}

func (transpiler *transpiler) setResults(file *file, arrowFunc *arrow, results []types.Type) bool {
	fields := make([]*ast.Field, len(results))
	for resultIndex, resultType := range results {
		resultExpr, ok := transpiler.arrowTypeExpr(file, arrowFunc, resultType, arrowFunc.funcLit.Pos())
		if !ok {
			return false
		}
		fields[resultIndex] = &ast.Field{Type: resultExpr}
	}
	arrowFunc.results, arrowFunc.resolved = results, true
	if len(results) > 0 {
		arrowFunc.funcLit.Type.Results = &ast.FieldList{List: fields}
		return true
	}
	arrowFunc.funcLit.Type.Results = nil
	if arrowFunc.bodyReturn != nil && len(arrowFunc.bodyReturn.Results) == 1 {
		arrowFunc.usedAsStmt = true
		arrowFunc.funcLit.Body.List[0] = &ast.ExprStmt{X: arrowFunc.bodyReturn.Results[0]}
	}
	return true
}

// arrowTypeExpr returns the expression for a type that arrowFunc needs. It
// reports an error if a local declaration at the arrow hides a name of the
// type, because the expression would then mean another type.
func (transpiler *transpiler) arrowTypeExpr(file *file, arrowFunc *arrow, goType types.Type, position token.Pos) (ast.Expr, bool) {
	typeExpr, ok := transpiler.typeExpr(file, goType, position)
	if !ok {
		return nil, false
	}
	if transpiler.isNameHidden(typeExpr, goType, arrowFunc.funcLit.Pos()) {
		arrowFunc.hasHiddenType = true
		transpiler.errorf(file, arrowFunc.funcLit.Pos(), "the inferred type %s cannot be written here, because a local declaration hides the name; rename the local declaration", displayName(goType))
		return nil, false
	}
	return typeExpr, true
}

// bodyResults infers the results of an arrow from its return statements.
// Each result takes the first type that is not untyped nil.
func (file *file) bodyResults(typeInfo *types.Info, arrowFunc *arrow) ([]types.Type, bool) {
	var returns []*ast.ReturnStmt
	ast.Inspect(arrowFunc.funcLit.Body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			if len(node.Results) > 0 {
				returns = append(returns, node)
			}
		}
		return true
	})
	if len(returns) == 0 {
		return nil, false
	}
	var resultTypes []types.Type
	for _, returnStmt := range returns {
		got, ok := returnTypes(typeInfo, returnStmt)
		if !ok {
			return nil, false
		}
		if resultTypes == nil {
			resultTypes = make([]types.Type, len(got))
		}
		for resultIndex := range min(len(resultTypes), len(got)) {
			if resultTypes[resultIndex] == nil && !isUntypedNil(got[resultIndex]) {
				resultTypes[resultIndex] = types.Default(got[resultIndex])
			}
		}
	}
	if slices.Contains(resultTypes, nil) {
		return nil, false // always nil, so the type is unknown
	}
	return resultTypes, true
}

func returnTypes(typeInfo *types.Info, returnStmt *ast.ReturnStmt) ([]types.Type, bool) {
	var resultTypes []types.Type
	for _, resultExpr := range returnStmt.Results {
		resultType := typeInfo.Types[resultExpr].Type
		if resultType == nil || resultType == types.Typ[types.Invalid] || hasPlaceholder(resultType) {
			return nil, false
		}
		if tuple, ok := resultType.(*types.Tuple); ok {
			for tupleVar := range tuple.Variables() {
				resultTypes = append(resultTypes, tupleVar.Type())
			}
			continue
		}
		resultTypes = append(resultTypes, resultType)
	}
	return resultTypes, true
}

// hasPlaceholder reports whether goType depends on an unresolved arrow.
// The placeholders are aliases, and the type checker keeps alias names.
func hasPlaceholder(goType types.Type) bool {
	typeText := types.TypeString(goType, nil)
	return strings.Contains(typeText, preprocess.ResultMarker) || strings.Contains(typeText, preprocess.UntypedMarker)
}

// arrowContext finds the function type expected where the arrow appears.
func (transpiler *transpiler) arrowContext(file *file, typeInfo *types.Info, parentOf map[ast.Node]ast.Node, arrowFunc *arrow) (funcContext, bool) {
	arrowExpr, parent := outermostParen(parentOf, arrowFunc.node())
	switch parentNode := parent.(type) {
	case *ast.CallExpr:
		if arrayCall, ok := file.arrayCalls[parentNode]; ok {
			return transpiler.callbackContext(typeInfo, parentNode, arrayCall, arrowFunc)
		}
		signature, ok := typeInfo.Types[parentNode.Fun].Type.(*types.Signature)
		if !ok || signature.TypeParams().Len() > 0 {
			return funcContext{}, false
		}
		for argIndex, arg := range parentNode.Args {
			if arg == arrowExpr {
				return expectFunc(paramType(signature, argIndex))
			}
		}
	case *ast.ValueSpec:
		for valueIndex, value := range parentNode.Values {
			if value == arrowExpr && parentNode.Type != nil && valueIndex < len(parentNode.Names) {
				return expectFunc(typeInfo.TypeOf(parentNode.Type))
			}
		}
	case *ast.AssignStmt:
		for rhsIndex, rightSide := range parentNode.Rhs {
			if rightSide == arrowExpr && parentNode.Tok == token.ASSIGN && len(parentNode.Lhs) == len(parentNode.Rhs) {
				return expectFunc(typeInfo.TypeOf(parentNode.Lhs[rhsIndex]))
			}
		}
	case *ast.ReturnStmt:
		signature := enclosingSignature(typeInfo, parentOf, parentNode)
		for resultIndex, result := range parentNode.Results {
			if result == arrowExpr && signature != nil && signature.Results().Len() == len(parentNode.Results) {
				return expectFunc(signature.Results().At(resultIndex).Type())
			}
		}
	}
	return funcContext{}, false
}

func expectFunc(goType types.Type) (funcContext, bool) {
	if goType == nil {
		return funcContext{}, false
	}
	signature, ok := goType.Underlying().(*types.Signature)
	if !ok {
		return funcContext{}, false
	}
	context := funcContext{resultsKnown: true}
	for param := range signature.Params().Variables() {
		context.params = append(context.params, param.Type())
	}
	for result := range signature.Results().Variables() {
		context.results = append(context.results, result.Type())
	}
	return context, true
}

// lowerArrows writes the resolved types over the arrow placeholders.
func (lowerer *lowerer) lowerArrows() {
	for _, arrowFunc := range lowerer.file.arrows {
		if arrowFunc.hasHiddenType {
			continue
		}
		if arrowFunc.nilStandIn != nil {
			lowerer.errorf(lowerer.file, arrowFunc.funcLit.Pos(), "cannot infer the parameter types of this arrow function; add types, as in (x int) => ...")
			continue
		}
		for paramIndex, placeholder := range arrowFunc.untyped {
			name, _ := lowerer.typeName(arrowFunc.params[paramIndex])
			lowerer.replace(placeholder.Pos(), placeholder.End(), name, 0)
		}
		if !arrowFunc.resolved && !arrowFunc.isVoidBlock {
			continue // the type checker already reported the error
		}
		names := make([]string, len(arrowFunc.results))
		for resultIndex, resultType := range arrowFunc.results {
			names[resultIndex], _ = lowerer.typeName(resultType)
		}
		result := strings.Join(names, ", ")
		if len(names) > 1 {
			result = "(" + result + ")"
		}
		lowerer.replace(arrowFunc.resultPlaceholder.Pos(), arrowFunc.resultPlaceholder.End(), result, 0)
		if arrowFunc.usedAsStmt {
			bodyExpr := arrowFunc.bodyReturn.Results[0]
			if parenExpr, ok := bodyExpr.(*ast.ParenExpr); ok {
				lowerer.replace(parenExpr.Rparen, parenExpr.Rparen+1, "", 0)
				bodyExpr = parenExpr.X
			}
			lowerer.replace(arrowFunc.bodyReturn.Return, bodyExpr.Pos(), "", 0)
		}
	}
}
