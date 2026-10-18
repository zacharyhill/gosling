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
// type checker can resolve. It sets each position in the expression to position.
func (transpiler *transpiler) typeExpr(file *file, goType types.Type, position token.Pos) (ast.Expr, bool) {
	typeText, ok := transpiler.typeNameIn(file, goType, true)
	if !ok {
		return nil, false
	}
	expr, err := parser.ParseExpr(typeText)
	if err != nil {
		return nil, false
	}
	placeAt(expr, position)
	return expr, true
}

// placeAt moves the positions in a parsed type expression to position. The
// parser gives them positions in no file, so errors show a wrong place.
func placeAt(typeExpr ast.Expr, position token.Pos) {
	ast.Inspect(typeExpr, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.Ident:
			node.NamePos = position
		case *ast.StarExpr:
			node.Star = position
		case *ast.ArrayType:
			node.Lbrack = position
		case *ast.MapType:
			node.Map = position
		case *ast.ChanType:
			node.Begin, node.Arrow = position, token.NoPos
		case *ast.IndexExpr:
			node.Lbrack, node.Rbrack = position, position
		case *ast.IndexListExpr:
			node.Lbrack, node.Rbrack = position, position
		case *ast.FuncType:
			node.Func = position
		case *ast.FieldList:
			node.Opening, node.Closing = position, position
		case *ast.StructType:
			node.Struct = position
		case *ast.InterfaceType:
			node.Interface = position
		case *ast.Ellipsis:
			node.Ellipsis = position
		case *ast.BasicLit:
			node.ValuePos = position
		}
		return true
	})
}

// isNameHidden reports whether typeExpr, read at position, does not mean
// goType because a local declaration hides one of its names.
func (transpiler *transpiler) isNameHidden(typeExpr ast.Expr, goType types.Type, position token.Pos) bool {
	if transpiler.checkedPackage == nil {
		return false
	}
	typeAndValue, err := types.Eval(transpiler.fileSet, transpiler.checkedPackage, position, types.ExprString(typeExpr))
	if err == nil {
		return !typeAndValue.IsType() || !types.Identical(typeAndValue.Type, goType)
	}
	// A package that the file does not import yet fails to evaluate. Only a
	// local name that hides a package name is an error.
	scope := transpiler.checkedPackage.Scope().Innermost(position)
	if scope == nil {
		return false
	}
	return isPackageNameHidden(typeExpr, scope, position)
}

// isPackageNameHidden reports whether a package name in typeExpr refers to a
// declaration that is not a package, as seen from scope at position.
func isPackageNameHidden(typeExpr ast.Expr, scope *types.Scope, position token.Pos) bool {
	isHidden := false
	ast.Inspect(typeExpr, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if packageIdent, ok := selector.X.(*ast.Ident); ok {
			_, object := scope.LookupParent(packageIdent.Name, position)
			_, isPackage := object.(*types.PkgName)
			isHidden = isHidden || (object != nil && !isPackage)
		}
		return true
	})
	return isHidden
}

// typeNameIn prints goType as file writes it. It imports the packages that
// the file does not have, also into the AST if inAST is true. It reports
// false if a package name is already used in the file.
func (transpiler *transpiler) typeNameIn(file *file, goType types.Type, inAST bool) (string, bool) {
	ok := true
	typeText := types.TypeString(goType, func(typePackage *types.Package) string {
		if typePackage.Path() == checkPath {
			return ""
		}
		packageName, found := transpiler.importName(file, typePackage.Path(), typePackage.Name(), inAST)
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
	return types.TypeString(goType, func(typePackage *types.Package) string {
		if typePackage.Path() == checkPath {
			return ""
		}
		return typePackage.Name()
	})
}

// typeNameTakenError reports that this file cannot write the type that a
// construct produces, because its package name is shadowed.
func (lowerer *lowerer) typeNameTakenError(position token.Pos, construct, typeText string) {
	lowerer.errorf(lowerer.file, position, "%s produces %s, whose package name is already used for something else in this file", construct, typeText)
}
