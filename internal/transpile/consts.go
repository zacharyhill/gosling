package transpile

import (
	"bytes"
	"go/ast"
	"go/token"
	"go/types"
	"slices"

	"golang.org/x/tools/go/ast/astutil"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// convertConsts turns each const declaration whose value is not constant
// into a variable that cannot be reassigned, as in TypeScript. A const with
// a constant value stays a Go constant. A grouped declaration stays Go. It
// reports whether the AST changed.
func convertConsts(file *file, typeInfo *types.Info) bool {
	changed := false
	astutil.Apply(file.ast, func(cursor *astutil.Cursor) bool {
		decl, spec := convertibleConst(cursor.Node(), typeInfo)
		if spec == nil {
			return true
		}
		for _, name := range spec.Names {
			file.constVars[name] = true
		}
		changed = true
		declStmt, inFunction := cursor.Parent().(*ast.DeclStmt)
		if !inFunction || spec.Type != nil {
			decl.Tok = token.VAR
			file.constDecls[decl] = nil
			return false
		}
		assign := &ast.AssignStmt{Tok: token.DEFINE, TokPos: file.assignPos(spec), Rhs: spec.Values}
		for _, name := range spec.Names {
			assign.Lhs = append(assign.Lhs, name)
		}
		file.constDecls[decl] = assign
		replaceNode(file.ast, declStmt, assign)
		return false
	}, nil)
	return changed
}

// recordConstNames adds the names at the offsets in preprocessed.Consts to
// constVars.
func (file *file) recordConstNames() {
	if len(file.preprocessed.Consts) == 0 {
		return
	}
	offsets := map[int]bool{}
	for _, offset := range file.preprocessed.Consts {
		offsets[offset] = true
	}
	ast.Inspect(file.ast, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok && offsets[file.tokenFile.Offset(ident.Pos())] {
			file.constVars[ident] = true
		}
		return true
	})
}

// convertibleConst returns node and its spec if node is a single const
// declaration. The value must type-check but not be constant, or the
// declared type must be one that a Go constant cannot have.
func convertibleConst(node ast.Node, typeInfo *types.Info) (*ast.GenDecl, *ast.ValueSpec) {
	decl, ok := node.(*ast.GenDecl)
	if !ok || decl.Tok != token.CONST || decl.Lparen.IsValid() || len(decl.Specs) != 1 {
		return nil, nil
	}
	spec := decl.Specs[0].(*ast.ValueSpec)
	if spec.Type != nil && len(spec.Values) > 0 && !isConstType(typeInfo.TypeOf(spec.Type)) {
		return decl, spec
	}
	constant := true
	for _, value := range spec.Values {
		typeAndValue, ok := typeInfo.Types[value]
		// A value with an unknown type may depend on a const that a later
		// pass converts.
		if !ok || typeAndValue.Type == nil || typeAndValue.Type == types.Typ[types.Invalid] {
			return nil, nil
		}
		constant = constant && typeAndValue.Value != nil
	}
	if constant {
		return nil, nil
	}
	return decl, spec
}

// isConstType reports whether a Go constant can have goType. An unknown
// type counts as one, so the type checker reports it.
func isConstType(goType types.Type) bool {
	if !isValidType(goType) {
		return true
	}
	basic, ok := goType.Underlying().(*types.Basic)
	return ok && basic.Info()&types.IsConstType != 0
}

// assignPos returns the position of the = in spec.
func (file *file) assignPos(spec *ast.ValueSpec) token.Pos {
	var afterNames ast.Node = spec.Names[len(spec.Names)-1]
	if spec.Type != nil {
		afterNames = spec.Type
	}
	start := file.tokenFile.Offset(afterNames.End())
	return afterNames.End() + token.Pos(bytes.IndexByte(file.preprocessed.Src[start:], '='))
}

// checkConstAssignments reports assignments to the variables in constVars.
func (transpiler *transpiler) checkConstAssignments(astFiles []*ast.File, typeInfo *types.Info) {
	constObjects := transpiler.constObjects(typeInfo)
	if len(constObjects) == 0 {
		return
	}
	for _, astFile := range astFiles {
		ast.Inspect(astFile, func(node ast.Node) bool {
			transpiler.reportConstTargets(assignTargets(node), constObjects, typeInfo)
			return true
		})
	}
}

func (transpiler *transpiler) reportConstTargets(targets []ast.Expr, constObjects map[types.Object]string, typeInfo *types.Info) {
	for _, target := range targets {
		ident, ok := ast.Unparen(target).(*ast.Ident)
		if !ok {
			continue
		}
		if reason, ok := constObjects[typeInfo.Uses[ident]]; ok {
			transpiler.diagnostics.Add(transpiler.position(ident.Pos()), "cannot assign to "+ident.Name+": "+reason)
		}
	}
}

// constObjects maps the objects of the variables that code cannot assign to
// the reason: a const declares them, or instanceof narrows them.
func (transpiler *transpiler) constObjects(typeInfo *types.Info) map[types.Object]string {
	constObjects := map[types.Object]string{}
	for _, file := range transpiler.files {
		for name := range file.constVars {
			if object := typeInfo.Defs[name]; object != nil {
				constObjects[object] = "declared with const"
			}
		}
		for name := range file.narrowedVars {
			if object := typeInfo.Defs[name]; object != nil {
				constObjects[object] = "narrowed by instanceof"
			}
		}
		addTypeSwitchObjects(file, typeInfo, constObjects)
	}
	return constObjects
}

// assignTargets returns the expressions that node assigns to.
func assignTargets(node ast.Node) []ast.Expr {
	switch node := node.(type) {
	case *ast.AssignStmt:
		return node.Lhs
	case *ast.IncDecStmt:
		return []ast.Expr{node.X}
	case *ast.RangeStmt:
		if node.Tok == token.ASSIGN {
			return []ast.Expr{node.Key, node.Value}
		}
	}
	return nil
}

// addTypeSwitchObjects adds the variables of each case of a type switch
// that a const declares. Each case has its own variable.
func addTypeSwitchObjects(file *file, typeInfo *types.Info, constObjects map[types.Object]string) {
	ast.Inspect(file.ast, func(node ast.Node) bool {
		typeSwitch, ok := node.(*ast.TypeSwitchStmt)
		if !ok {
			return true
		}
		assign, ok := typeSwitch.Assign.(*ast.AssignStmt)
		if !ok || !file.constVars[assign.Lhs[0].(*ast.Ident)] {
			return true
		}
		for _, clause := range typeSwitch.Body.List {
			if object := typeInfo.Implicits[clause]; object != nil {
				constObjects[object] = "declared with const"
			}
		}
		return true
	})
}

// lowerConsts writes the declarations that convertConsts changed.
func (lowerer *lowerer) lowerConsts() {
	for decl, assign := range lowerer.file.constDecls {
		line := lowerer.line(decl.TokPos)
		if assign == nil {
			lowerer.replace(decl.TokPos, decl.TokPos+token.Pos(len("const")), "var", line)
			continue
		}
		lowerer.replace(decl.TokPos, assign.Lhs[0].Pos(), "", line)
		if !lowerer.file.blankDefines[assign] {
			lowerer.replace(assign.TokPos, assign.TokPos+1, ":=", line)
		}
	}
}

// convertBlankDefines turns each := whose names are all blank into =. It
// reports whether it changed one.
func (file *file) convertBlankDefines() bool {
	changed := false
	ast.Inspect(file.ast, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if ok && assign.Tok == token.DEFINE && isAllBlank(assign.Lhs) {
			assign.Tok = token.ASSIGN
			file.blankDefines[assign] = true
			changed = true
		}
		return true
	})
	return changed
}

func isAllBlank(names []ast.Expr) bool {
	return !slices.ContainsFunc(names, func(name ast.Expr) bool {
		ident, ok := name.(*ast.Ident)
		return !ok || ident.Name != "_"
	})
}

// lowerBlankDefines writes = for the := of each let in blankDefines. A
// const already has = in the source, and lowerConsts keeps it.
func (lowerer *lowerer) lowerBlankDefines() {
	for assign := range lowerer.file.blankDefines {
		if bytes.HasPrefix(lowerer.file.preprocessed.Src[lowerer.offset(assign.TokPos):], []byte(":=")) {
			lowerer.replace(assign.TokPos, assign.TokPos+token.Pos(len(":=")), "=", lowerer.line(assign.TokPos))
		}
	}
}

// lowerTypedHeaders turns each typed header declaration, x :=
// __goslingTyped[T](v), into x := T(v). The type checker already checked that
// v is assignable to T, so the conversion does not change the value.
func (lowerer *lowerer) lowerTypedHeaders() {
	ast.Inspect(lowerer.file.ast, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		markerIndex, ok := call.Fun.(*ast.IndexExpr)
		if !ok {
			return true
		}
		if marker, ok := markerIndex.X.(*ast.Ident); !ok || marker.Name != preprocess.TypedMarker {
			return true
		}
		line := lowerer.line(call.Pos())
		openParen, closeParen := "", ""
		if shouldParenthesizeType(markerIndex.Index) {
			openParen, closeParen = "(", ")"
		}
		lowerer.replace(markerIndex.X.Pos(), markerIndex.Index.Pos(), openParen, line)
		lowerer.replace(markerIndex.Index.End(), call.Lparen, closeParen, line)
		return true
	})
}

// shouldParenthesizeType reports whether a conversion to typeExpr needs
// parentheses around the type, as in (*T)(v) or (func())(v).
func shouldParenthesizeType(typeExpr ast.Expr) bool {
	switch typeExpr.(type) {
	case *ast.Ident, *ast.SelectorExpr, *ast.IndexExpr, *ast.IndexListExpr, *ast.ArrayType, *ast.MapType:
		return false
	}
	return true
}
