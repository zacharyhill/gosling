package transpile

import (
	"go/ast"
	"go/types"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// recordOptionalFields finds the names of the struct fields that the source
// declares with ?.
func (file *file) recordOptionalFields() {
	if len(file.preprocessed.Optionals) == 0 {
		return
	}
	ast.Inspect(file.ast, func(node ast.Node) bool {
		field, ok := node.(*ast.Field)
		if !ok {
			return true
		}
		for _, name := range field.Names {
			if slices.Contains(file.preprocessed.Optionals, file.tokenFile.Offset(name.Pos())) {
				file.optionalFields = append(file.optionalFields, name)
			}
		}
		return true
	})
}

// collectOptionalFieldVars returns the optional fields of all files as
// objects of the last type-check pass.
func (transpiler *transpiler) collectOptionalFieldVars(typeInfo *types.Info) map[types.Object]bool {
	fieldVars := map[types.Object]bool{}
	for _, file := range transpiler.files {
		for _, name := range file.optionalFields {
			if fieldVar := typeInfo.Defs[name]; fieldVar != nil {
				fieldVars[fieldVar] = true
			}
		}
	}
	return fieldVars
}

// addressOptionalValues wraps each value of an optional field in a composite
// literal if the value has the element type, as in Config{Host: "x"}. It
// reports whether anything changed.
func (transpiler *transpiler) addressOptionalValues(file *file, typeInfo *types.Info, fieldVars map[types.Object]bool) bool {
	if len(fieldVars) == 0 {
		return false
	}
	changed := false
	ast.Inspect(file.ast, func(node ast.Node) bool {
		compositeLit, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}
		structType, ok := underlyingStruct(typeInfo.Types[compositeLit].Type)
		if !ok {
			return true
		}
		keyed := hasKeyedElement(compositeLit)
		for elementIndex, element := range compositeLit.Elts {
			fieldVar, value := findElementField(typeInfo, structType, element, elementIndex, keyed)
			// A field of an instantiated generic type has its own object.
			if fieldVar == nil || !fieldVars[fieldVar.Origin()] {
				continue
			}
			marker, ok := transpiler.addressValue(file, typeInfo, fieldVar.Type(), value)
			if !ok {
				continue
			}
			if keyValue, isKeyValue := element.(*ast.KeyValueExpr); isKeyValue {
				keyValue.Value = marker
			} else {
				compositeLit.Elts[elementIndex] = marker
			}
			changed = true
		}
		return true
	})
	return changed
}

// findElementField returns the field that element sets and the value it sets.
func findElementField(typeInfo *types.Info, structType *types.Struct, element ast.Expr, elementIndex int, keyed bool) (*types.Var, ast.Expr) {
	if keyValue, ok := element.(*ast.KeyValueExpr); ok {
		key, isIdent := keyValue.Key.(*ast.Ident)
		if !isIdent {
			return nil, nil
		}
		fieldVar, _ := typeInfo.Uses[key].(*types.Var)
		return fieldVar, keyValue.Value
	}
	if keyed || elementIndex >= structType.NumFields() {
		return nil, nil
	}
	return structType.Field(elementIndex), element
}

// addressValue wraps value in the address marker if value has the element
// type of fieldType but cannot be assigned to fieldType itself.
func (transpiler *transpiler) addressValue(file *file, typeInfo *types.Info, fieldType types.Type, value ast.Expr) (*ast.CallExpr, bool) {
	pointerType, ok := fieldType.(*types.Pointer)
	valueType := typeInfo.Types[value].Type
	if !ok || valueType == nil || types.AssignableTo(valueType, fieldType) || !types.AssignableTo(valueType, pointerType.Elem()) {
		return nil, false
	}
	elementExpr, ok := transpiler.typeExpr(file, pointerType.Elem(), value.Pos())
	if !ok {
		return nil, false
	}
	marker := &ast.CallExpr{
		Fun:    &ast.IndexExpr{X: &ast.Ident{Name: preprocess.AddrMarker, NamePos: value.Pos()}, Index: elementExpr},
		Args:   []ast.Expr{value},
		Rparen: value.End() - 1,
	}
	file.addressed[marker] = true
	return marker, true
}

// lowerOptionalValues writes each address marker as a function literal that
// returns a pointer to a copy of its argument.
func (lowerer *lowerer) lowerOptionalValues() {
	for marker := range lowerer.file.addressed {
		pointerType, ok := lowerer.typeInfo.Types[marker].Type.(*types.Pointer)
		if !ok {
			continue // the type checker already reported the error
		}
		value := marker.Args[0]
		line := lowerer.line(value.Pos())
		typeText, ok := lowerer.typeName(pointerType.Elem())
		if !ok {
			lowerer.typeNameTakenError(value.Pos(), "an optional field", displayName(pointerType.Elem()))
			continue
		}
		paramName := pickParamName(typeText)
		lowerer.insertOrdered(value.Pos(), "func("+paramName+" "+typeText+") *"+typeText+" { return &"+paramName+" }(", line, 1)
		lowerer.insert(value.End(), ")", line)
	}
}

// pickParamName returns a parameter name that does not occur in typeText.
func pickParamName(typeText string) string {
	words := strings.FieldsFunc(typeText, func(char rune) bool {
		return !unicode.IsLetter(char) && !unicode.IsDigit(char) && char != '_'
	})
	for attempt := 1; ; attempt++ {
		name := "v"
		if attempt > 1 {
			name = "v" + strconv.Itoa(attempt)
		}
		if !slices.Contains(words, name) {
			return name
		}
	}
}
