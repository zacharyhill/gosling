package transpile

import (
	"go/ast"
	"go/types"
	"sort"

	"golang.org/x/tools/go/ast/astutil"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// A ternary type-checks as a call to a generic marker, so both arms must
// have one type T. It lowers to an immediately called function literal, so
// only the chosen arm runs:
//
//	c ? a : b
//
// becomes
//
//	func() T {
//		if c {
//			return a
//		}
//		return b
//	}()
//
// A ternary used as a statement becomes an if statement in the AST, so its
// arms can be calls without a value.
const ternaryMarker = preprocess.TernaryMarker

const ternaryMarkerDecl = "func " + ternaryMarker + "[T any](cond bool, then, otherwise T) T { return then }\n"

func (file *file) recordTernaries() {
	astutil.Apply(file.ast, nil, func(cursor *astutil.Cursor) bool {
		if stmt, ok := cursor.Node().(*ast.ExprStmt); ok {
			if call, ok := callTo(stmt.X, ternaryMarker); ok {
				cursor.Replace(file.buildTernaryIf(call))
			}
		}
		return true
	})
	ast.Inspect(file.ast, func(node ast.Node) bool {
		if expr, ok := node.(ast.Expr); ok {
			if call, ok := callTo(expr, ternaryMarker); ok {
				file.ternaries[call] = true
			}
		}
		return true
	})
}

// buildTernaryIf returns the if statement for a ternary used as a statement. A
// ternary in an arm becomes a nested if or an else if.
func (file *file) buildTernaryIf(call *ast.CallExpr) *ast.IfStmt {
	cond, then, otherwise := call.Args[0], call.Args[1], call.Args[2]
	ifStmt := &ast.IfStmt{
		If:   call.Pos(),
		Cond: cond,
		Body: &ast.BlockStmt{Lbrace: cond.End(), List: []ast.Stmt{file.buildTernaryArm(then)}, Rbrace: then.End()},
	}
	if inner, ok := callTo(otherwise, ternaryMarker); ok {
		ifStmt.Else = file.buildTernaryIf(inner)
	} else {
		ifStmt.Else = &ast.BlockStmt{Lbrace: then.End(), List: []ast.Stmt{&ast.ExprStmt{X: otherwise}}, Rbrace: call.Rparen}
	}
	file.ternaryStmts[ifStmt] = call
	return ifStmt
}

func (file *file) buildTernaryArm(arm ast.Expr) ast.Stmt {
	if inner, ok := callTo(arm, ternaryMarker); ok {
		return file.buildTernaryIf(inner)
	}
	return &ast.ExprStmt{X: arm}
}

// fixTernaryTypes gives a ternary an explicit type when inference cannot
// find a good one. It reports whether anything changed.
func (transpiler *transpiler) fixTernaryTypes(file *file, typeInfo *types.Info) bool {
	changed := false
	parentOf := parents(file.ast)
	for call := range file.ternaries {
		marker, ok := call.Fun.(*ast.Ident)
		if !ok || len(call.Args) != 3 {
			continue // already explicit
		}
		resultType := pickTernaryType(typeInfo, call, contextType(typeInfo, parentOf, call))
		if resultType == nil {
			continue
		}
		typeExpr, ok := transpiler.typeExpr(file, resultType, call.Pos())
		if !ok {
			continue
		}
		call.Fun = &ast.IndexExpr{X: marker, Lbrack: marker.End(), Index: typeExpr, Rbrack: marker.End()}
		changed = true
	}
	return changed
}

// pickTernaryType returns the type for a ternary whose arms do not infer the
// right type, or nil. The context type wins if inference fails, or if both
// arms are constants and the context is not an interface. Otherwise one arm
// must be assignable to the type of the other.
func pickTernaryType(typeInfo *types.Info, call *ast.CallExpr, context types.Type) types.Type {
	then, otherwise := typeInfo.Types[call.Args[1]], typeInfo.Types[call.Args[2]]
	if !isValidType(then.Type) || !isValidType(otherwise.Type) {
		return nil // the type checker reports the arm
	}
	inferred := typeInfo.Types[call].Type
	isInferred := isValidType(inferred)
	isConstant := then.Value != nil && otherwise.Value != nil
	if context != nil && (!isInferred || isConstant && !types.IsInterface(context) && !types.Identical(context, inferred)) {
		return context
	}
	if isInferred || isUntypedNil(then.Type) || isUntypedNil(otherwise.Type) {
		return nil
	}
	thenType, otherwiseType := types.Default(then.Type), types.Default(otherwise.Type)
	switch {
	case types.AssignableTo(otherwiseType, thenType):
		return thenType
	case types.AssignableTo(thenType, otherwiseType):
		return otherwiseType
	}
	return nil
}

func isValidType(goType types.Type) bool {
	return goType != nil && goType != types.Typ[types.Invalid]
}

// lowerTernaries writes each ternary as a function literal, or as an if
// statement if it is a statement.
func (lowerer *lowerer) lowerTernaries() {
	for _, call := range lowerer.file.ternaryStmts {
		lowerer.lowerTernaryStmt(call)
	}
	calls := make([]*ast.CallExpr, 0, len(lowerer.file.ternaries))
	for call := range lowerer.file.ternaries {
		calls = append(calls, call)
	}
	sort.Slice(calls, func(left, right int) bool { return calls[left].Pos() < calls[right].Pos() })
	for _, call := range calls {
		if lowerer.isChainedTernary(call) {
			lowerer.lowerTernary(call, "if ", "")
			continue
		}
		resultType := lowerer.typeInfo.Types[call].Type
		typeText, ok := lowerer.typeName(resultType)
		if !ok {
			lowerer.typeNameTakenError(call.Pos(), "ternary", typeText)
			continue
		}
		lowerer.lowerTernary(call, "func() "+typeText+" {\nif ", "\n}()")
	}
}

// isChainedTernary reports whether call is the else arm of another ternary
// with the same type. Its if statement then joins the function literal of
// the outer ternary, so a chain does not nest.
func (lowerer *lowerer) isChainedTernary(call *ast.CallExpr) bool {
	outer, ok := lowerer.parents[call].(*ast.CallExpr)
	return ok && lowerer.file.ternaries[outer] && outer.Args[2] == call &&
		types.Identical(lowerer.typeInfo.Types[outer].Type, lowerer.typeInfo.Types[call].Type)
}

func (lowerer *lowerer) lowerTernary(call *ast.CallExpr, open, closing string) {
	cond, then, otherwise := call.Args[0], call.Args[1], call.Args[2]
	line := lowerer.line(call.Pos())
	lowerer.replace(call.Pos(), cond.Pos(), open, line)
	lowerer.replace(cond.End(), then.Pos(), " {\nreturn ", line)
	elseText := "\n}\nreturn "
	if inner, ok := otherwise.(*ast.CallExpr); ok && lowerer.file.ternaries[inner] && lowerer.isChainedTernary(inner) {
		elseText = "\n}\n"
	}
	lowerer.replace(then.End(), otherwise.Pos(), elseText, line)
	lowerer.replace(otherwise.End(), call.Rparen+1, closing, line)
}

func (lowerer *lowerer) lowerTernaryStmt(call *ast.CallExpr) {
	cond, then, otherwise := call.Args[0], call.Args[1], call.Args[2]
	line := lowerer.line(call.Pos())
	lowerer.replace(call.Pos(), cond.Pos(), "if ", line)
	lowerer.replace(cond.End(), then.Pos(), " {\n", line)
	if _, isElseIf := callTo(otherwise, ternaryMarker); isElseIf {
		lowerer.replace(then.End(), otherwise.Pos(), "\n} else ", line)
		lowerer.replace(otherwise.End(), call.Rparen+1, "", line)
		return
	}
	lowerer.replace(then.End(), otherwise.Pos(), "\n} else {\n", line)
	lowerer.replace(otherwise.End(), call.Rparen+1, "\n}", line)
}
