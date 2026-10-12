package transpile

import (
	"bytes"
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/ast/astutil"
)

// convertConsts turns each const declaration whose value is not constant
// into a variable that cannot be reassigned, as in TypeScript. A const with
// a constant value stays a Go constant. A grouped declaration stays Go. It
// reports whether the AST changed.
func convertConsts(file *file, info *types.Info) bool {
	changed := false
	astutil.Apply(file.ast, func(cursor *astutil.Cursor) bool {
		decl, spec := convertibleConst(cursor.Node(), info)
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
// declaration with a value that type-checks but is not constant.
func convertibleConst(node ast.Node, info *types.Info) (*ast.GenDecl, *ast.ValueSpec) {
	decl, ok := node.(*ast.GenDecl)
	if !ok || decl.Tok != token.CONST || decl.Lparen.IsValid() || len(decl.Specs) != 1 {
		return nil, nil
	}
	spec := decl.Specs[0].(*ast.ValueSpec)
	constant := true
	for _, value := range spec.Values {
		typeAndValue, ok := info.Types[value]
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
func (transpiler *transpiler) checkConstAssignments(astFiles []*ast.File, info *types.Info) {
	constObjects := map[types.Object]bool{}
	for _, file := range transpiler.files {
		for name := range file.constVars {
			if object := info.Defs[name]; object != nil {
				constObjects[object] = true
			}
		}
		addTypeSwitchObjects(file, info, constObjects)
	}
	if len(constObjects) == 0 {
		return
	}
	report := func(targets ...ast.Expr) {
		for _, target := range targets {
			ident, ok := ast.Unparen(target).(*ast.Ident)
			if ok && constObjects[info.Uses[ident]] {
				transpiler.diagnostics.Add(transpiler.position(ident.Pos()), "cannot assign to "+ident.Name+": declared with const")
			}
		}
	}
	for _, astFile := range astFiles {
		ast.Inspect(astFile, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.AssignStmt:
				report(node.Lhs...)
			case *ast.IncDecStmt:
				report(node.X)
			case *ast.RangeStmt:
				if node.Tok == token.ASSIGN {
					report(node.Key, node.Value)
				}
			}
			return true
		})
	}
}

// addTypeSwitchObjects adds the variables of each case of a type switch
// that a const declares. Each case has its own variable.
func addTypeSwitchObjects(file *file, info *types.Info, constObjects map[types.Object]bool) {
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
			if object := info.Implicits[clause]; object != nil {
				constObjects[object] = true
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
		lowerer.replace(assign.TokPos, assign.TokPos+1, ":=", line)
	}
}
