package transpile

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
)

func zeroValue(goType types.Type, typeText string) string {
	if _, ok := goType.(*types.TypeParam); ok {
		return "*new(" + typeText + ")"
	}
	switch underlying := goType.Underlying().(type) {
	case *types.Basic:
		switch {
		case underlying.Info()&types.IsBoolean != 0:
			return "false"
		case underlying.Info()&types.IsString != 0:
			return `""`
		case underlying.Info()&types.IsNumeric != 0:
			return "0"
		case underlying.Kind() == types.UnsafePointer:
			return "nil"
		}
	case *types.Pointer, *types.Slice, *types.Map, *types.Chan, *types.Signature, *types.Interface:
		return "nil"
	case *types.Struct, *types.Array:
		return typeText + "{}"
	}
	return "*new(" + typeText + ")"
}

func nilable(goType types.Type) bool {
	if goType == nil {
		return false
	}
	switch goType.Underlying().(type) {
	case *types.Pointer, *types.Interface, *types.Map, *types.Slice, *types.Signature, *types.Chan:
		return true
	}
	return false
}

// typeName prints goType as this file writes it. It reports false if the
// type belongs to a package that the file does not import.
func (lowerer *lowerer) typeName(goType types.Type) (string, bool) {
	return lowerer.typeNameIn(lowerer.file, goType, false)
}

func isUntypedNil(goType types.Type) bool {
	basic, ok := goType.(*types.Basic)
	return ok && basic.Kind() == types.UntypedNil
}

func paramType(signature *types.Signature, paramIndex int) types.Type {
	paramCount := signature.Params().Len()
	if signature.Variadic() && paramIndex >= paramCount-1 {
		if variadicSlice, ok := signature.Params().At(paramCount - 1).Type().(*types.Slice); ok {
			return variadicSlice.Elem()
		}
		return nil
	}
	if paramIndex < paramCount {
		return signature.Params().At(paramIndex).Type()
	}
	return nil
}

// typeExpr parses goType, as file writes it, into an expression that the
// type checker can resolve.
func (transpiler *transpiler) typeExpr(file *file, goType types.Type) (ast.Expr, bool) {
	typeText, ok := transpiler.typeNameIn(file, goType, true)
	if !ok {
		return nil, false
	}
	expr, err := parser.ParseExpr(typeText)
	return expr, err == nil
}

// typeNameIn prints goType as file writes it. It imports the packages that
// the file does not have, also into the AST if inAST is true. It reports
// false if a package name is already used in the file.
func (transpiler *transpiler) typeNameIn(file *file, goType types.Type, inAST bool) (string, bool) {
	ok := true
	typeText := types.TypeString(goType, func(pkg *types.Package) string {
		if pkg.Path() == checkPath {
			return ""
		}
		packageName, found := transpiler.importName(file, pkg.Path(), pkg.Name(), inAST)
		ok = ok && found
		return packageName
	})
	return typeText, ok
}

func underlyingSlice(goType types.Type) (*types.Slice, bool) {
	if goType == nil {
		return nil, false
	}
	sliceType, ok := goType.Underlying().(*types.Slice)
	return sliceType, ok
}

func underlyingStruct(goType types.Type) (*types.Struct, bool) {
	if goType == nil {
		return nil, false
	}
	if pointer, ok := goType.Underlying().(*types.Pointer); ok {
		goType = pointer.Elem()
	}
	structType, ok := goType.Underlying().(*types.Struct)
	return structType, ok
}

func containsType(list []types.Type, goType types.Type) bool {
	for _, candidate := range list {
		if candidate != nil && types.Identical(candidate, goType) {
			return true
		}
	}
	return false
}

// displayName prints a type for an error message.
func displayName(goType types.Type) string {
	return types.TypeString(goType, func(pkg *types.Package) string {
		if pkg.Path() == checkPath {
			return ""
		}
		return pkg.Name()
	})
}

// typeNameTakenError reports that this file cannot write the type that a
// construct produces, because its package name is shadowed.
func (lowerer *lowerer) typeNameTakenError(pos token.Pos, construct, typeText string) {
	lowerer.errorf(lowerer.file, pos, "%s produces %s, whose package name is already used for something else in this file", construct, typeText)
}
