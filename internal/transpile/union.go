package transpile

import (
	"go/ast"
	"go/types"
	"strings"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// collectUnions resolves the variants of each union from its marker
// function. It checks that the variants can carry the marker method of the
// union. It returns the source offsets of unions with bad variants, by file.
func (transpiler *transpiler) collectUnions() map[*file]map[int]bool {
	transpiler.unions = map[*types.TypeName][]types.Type{}
	badUnions := map[*file]map[int]bool{}
	if transpiler.checkedPackage == nil {
		return badUnions
	}
	for _, sourceFile := range transpiler.files {
		for _, union := range sourceFile.preprocessed.Unions {
			unionType, ok := transpiler.checkedPackage.Scope().Lookup(union.Name).(*types.TypeName)
			marker, isFunc := transpiler.checkedPackage.Scope().Lookup(preprocess.VariantsPrefix + union.Name).(*types.Func)
			if !ok || !isFunc {
				continue
			}
			pos := sourceFile.tokenFile.Pos(union.MarkerStart)
			var variants []types.Type
			for variant := range marker.Signature().Params().Variables() {
				if message := transpiler.badVariant(variant.Type()); message != "" {
					transpiler.errorf(sourceFile, pos, "%s", message)
					if badUnions[sourceFile] == nil {
						badUnions[sourceFile] = map[int]bool{}
					}
					badUnions[sourceFile][sourceFile.preprocessed.OrigOffset(union.MarkerStart)] = true
				}
				variants = append(variants, variant.Type())
			}
			transpiler.unions[unionType] = variants
		}
	}
	return badUnions
}

func (transpiler *transpiler) badVariant(variant types.Type) string {
	name := displayName(variant)
	base := variant
	if pointer, ok := variant.(*types.Pointer); ok {
		base = pointer.Elem()
	}
	named, ok := types.Unalias(base).(*types.Named)
	switch {
	case !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != checkPath:
		return "union variant " + name + " must be a type declared in this package"
	case types.IsInterface(named):
		return "union variant " + name + " must not be an interface"
	}
	return ""
}

// checkExhaustive reports a type switch on a union that does not list each
// variant and has no default case.
func (lowerer *lowerer) checkExhaustive(typeSwitch *ast.TypeSwitchStmt) {
	union, variants, ok := lowerer.switchedUnion(typeSwitch)
	if !ok {
		return
	}
	covered, hasDefault := lowerer.caseTypes(typeSwitch)
	if hasDefault {
		return
	}
	var missing []string
	for _, variant := range variants {
		if !containsType(covered, variant) {
			missing = append(missing, displayName(variant))
		}
	}
	if len(missing) > 0 {
		lowerer.errorf(lowerer.file, typeSwitch.Pos(), "switch on %s is missing cases: %s", union.Obj().Name(), strings.Join(missing, ", "))
	}
}

// switchedUnion returns the union type that a type switch is on, and its variants.
func (lowerer *lowerer) switchedUnion(typeSwitch *ast.TypeSwitchStmt) (*types.Named, []types.Type, bool) {
	var subject ast.Expr
	switch assign := typeSwitch.Assign.(type) {
	case *ast.ExprStmt:
		subject = assign.X
	case *ast.AssignStmt:
		subject = assign.Rhs[0]
	}
	assert, ok := subject.(*ast.TypeAssertExpr)
	if !ok {
		return nil, nil, false
	}
	named, ok := types.Unalias(lowerer.info.Types[assert.X].Type).(*types.Named)
	if !ok {
		return nil, nil, false
	}
	variants, ok := lowerer.unions[named.Obj()]
	return named, variants, ok
}

func (lowerer *lowerer) caseTypes(typeSwitch *ast.TypeSwitchStmt) ([]types.Type, bool) {
	var covered []types.Type
	for _, clauseStmt := range typeSwitch.Body.List {
		clause := clauseStmt.(*ast.CaseClause)
		if clause.List == nil {
			return nil, true
		}
		for _, expr := range clause.List {
			covered = append(covered, lowerer.info.Types[expr].Type)
		}
	}
	return covered, false
}

// lowerUnions removes the marker functions from the output.
func (lowerer *lowerer) lowerUnions() {
	for _, union := range lowerer.file.preprocessed.Unions {
		lowerer.edits = append(lowerer.edits, edit{start: union.MarkerStart, end: union.MarkerEnd})
	}
}
