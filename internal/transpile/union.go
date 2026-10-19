package transpile

import (
	"fmt"
	"go/ast"
	"go/types"
	"slices"
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
			if transpiler.collectUnion(sourceFile, union) {
				continue
			}
			if badUnions[sourceFile] == nil {
				badUnions[sourceFile] = map[int]bool{}
			}
			badUnions[sourceFile][sourceFile.preprocessed.OrigOffset(union.MarkerStart)] = true
		}
	}
	return badUnions
}

// collectUnion records the variants of union and reports each bad variant.
// It reports false if a variant is bad.
func (transpiler *transpiler) collectUnion(sourceFile *file, union preprocess.Union) bool {
	unionType, ok := transpiler.checkedPackage.Scope().Lookup(union.Name).(*types.TypeName)
	marker, isFunc := transpiler.checkedPackage.Scope().Lookup(preprocess.VariantsPrefix + union.Name).(*types.Func)
	if !ok || !isFunc {
		return true
	}
	position := sourceFile.tokenFile.Pos(union.MarkerStart)
	isValid := true
	var variants []types.Type
	for variant := range marker.Signature().Params().Variables() {
		if message := transpiler.badVariant(variant.Type()); message != "" {
			transpiler.errorf(sourceFile, position, "%s", message)
			isValid = false
		}
		variants = append(variants, variant.Type())
	}
	transpiler.unions[unionType] = variants
	return isValid
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
		instance, err := instantiateVariant(variant, union.TypeArgs())
		if err != nil {
			lowerer.errorf(lowerer.file, typeSwitch.Pos(), "cannot check that the switch on %s covers %s: %s", displayName(union), displayName(variant), strings.ReplaceAll(err.Error(), checkPath+".", ""))
			return
		}
		if !containsType(covered, instance) {
			missing = append(missing, displayName(instance))
		}
	}
	if len(missing) > 0 {
		lowerer.errorf(lowerer.file, typeSwitch.Pos(), "switch on %s is missing cases: %s", displayName(union), strings.Join(missing, ", "))
	}
}

// instantiateVariant replaces the type parameters of a generic union in
// variant with typeArgs, the type arguments of a union instance. The
// variant has the type parameters as its type arguments, in order. It fails
// only for a union with type errors, which KeepGoing still lowers.
func instantiateVariant(variant types.Type, typeArgs *types.TypeList) (types.Type, error) {
	if typeArgs.Len() == 0 {
		return variant, nil
	}
	base := variant
	pointer, isPointer := variant.(*types.Pointer)
	if isPointer {
		base = pointer.Elem()
	}
	named, ok := types.Unalias(base).(*types.Named)
	if !ok {
		return nil, fmt.Errorf("%s is not a named type", displayName(variant))
	}
	instance, err := types.Instantiate(nil, named.Origin(), slices.Collect(typeArgs.Types()), true)
	if err != nil {
		return nil, err
	}
	if isPointer {
		return types.NewPointer(instance), nil
	}
	return instance, nil
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
	named, ok := types.Unalias(lowerer.typeInfo.Types[assert.X].Type).(*types.Named)
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
			covered = append(covered, lowerer.typeInfo.Types[expr].Type)
		}
	}
	return covered, false
}

// lowerUnions removes the marker functions from the output.
func (lowerer *lowerer) lowerUnions() {
	for _, union := range lowerer.file.preprocessed.Unions {
		lowerer.replaceOffsets(union.MarkerStart, union.MarkerEnd, "", 0)
	}
}
