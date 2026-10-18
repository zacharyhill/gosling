package transpile

import (
	"go/ast"
	"go/types"
	"regexp"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// defaultPtrMarker marks a default on a pointer field. The result is the
// dereferenced value, as with ??.
const defaultPtrMarker = "__goslingDefaultPtr"

var defaultMarkerDecls = "func " + preprocess.DefaultMarker + "[T any](x T, d T) T { return d }\n" +
	"func " + defaultPtrMarker + "[T any](x *T, d T) T { return d }\n" +
	"func " + preprocess.DefaultAtMarker + "[T any](ok bool, v T, d T) T { return d }\n"

var defaultMarkerRE = regexp.MustCompile(`in (argument to|call to) ` + preprocess.DefaultMarker + `(Ptr|At)?`)

// useDefaultPtr switches a default on a pointer field to the pointer form if
// the default has the element type. It reports whether anything changed.
func useDefaultPtr(file *file, typeInfo *types.Info) bool {
	changed := false
	ast.Inspect(file.ast, func(node ast.Node) bool {
		call, ok := callTo(asExpr(node), preprocess.DefaultMarker)
		if !ok || len(call.Args) != 2 {
			return true
		}
		fieldType, defaultValue := typeInfo.Types[call.Args[0]].Type, typeInfo.Types[call.Args[1]]
		if _, isPointer := fieldType.(*types.Pointer); !isPointer || defaultValue.Type == nil {
			return true
		}
		// The checker records a constant with its converted type, so check
		// for any constant, not only an untyped one.
		if defaultValue.Value != nil || !types.AssignableTo(defaultValue.Type, fieldType) {
			call.Fun.(*ast.Ident).Name = defaultPtrMarker
			changed = true
		}
		return true
	})
	return changed
}

func asExpr(node ast.Node) ast.Expr {
	expr, _ := node.(ast.Expr)
	return expr
}

// isDefaultMarker reports whether call is a default marker. A call in its
// last argument runs only if the default applies.
func isDefaultMarker(call *ast.CallExpr) bool {
	ident, ok := call.Fun.(*ast.Ident)
	return ok && (ident.Name == preprocess.DefaultMarker || ident.Name == defaultPtrMarker || ident.Name == preprocess.DefaultAtMarker)
}

// lowerDefaults writes each default marker as a closure. A default applies if
// the value is its zero value, or if an array pattern reads past the end of a
// slice. The default runs only then:
//
//	{Port = 8080} = cfg
//
// becomes
//
//	func() int {
//		if v := cfg.Port; v != 0 {
//			return v
//		}
//		return 8080
//	}()
func (lowerer *lowerer) lowerDefaults() {
	ast.Inspect(lowerer.file.ast, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isDefaultMarker(call) || lowerer.dropped(call.Pos()) {
			return true
		}
		resultType := lowerer.typeInfo.Types[call].Type
		if resultType == nil {
			return true // the type checker already reported the error
		}
		typeText, ok := lowerer.typeName(resultType)
		if !ok {
			lowerer.typeNameTakenError(call.Pos(), "destructuring default", typeText)
			return true
		}
		closureHeader := "func() " + typeText + " {\n"
		if call.Fun.(*ast.Ident).Name == preprocess.DefaultAtMarker {
			lowerer.lowerDefaultAt(call, closureHeader)
		} else {
			lowerer.lowerDefault(call, closureHeader)
		}
		return true
	})
}

// lowerDefaultAt writes __goslingDefaultAt(ok, v, d) as
// func() T { if ok { return v }; return d }().
func (lowerer *lowerer) lowerDefaultAt(call *ast.CallExpr, closureHeader string) {
	inBounds, value, fallback := call.Args[0], call.Args[1], call.Args[2]
	line := lowerer.line(call.Pos())
	lowerer.replace(call.Pos(), inBounds.Pos(), closureHeader+"if ", line)
	lowerer.replace(inBounds.End(), value.Pos(), " {\nreturn ", line)
	lowerer.replace(value.End(), fallback.Pos(), "\n}\nreturn ", line)
	lowerer.replace(fallback.End(), call.End(), "\n}()", line)
}

// lowerDefault writes __goslingDefault(x, d) as
// func() T { if v := x; v != zero { return v }; return d }().
func (lowerer *lowerer) lowerDefault(call *ast.CallExpr, closureHeader string) {
	field, fallback := call.Args[0], call.Args[1]
	fieldType := lowerer.typeInfo.Types[field].Type
	tempVar := lowerer.pickTempVar(call.Pos(), call.End())
	condition, result := tempVar+" != nil", tempVar
	switch {
	case call.Fun.(*ast.Ident).Name == defaultPtrMarker:
		result = "*" + tempVar
	case nilable(fieldType):
	case types.Comparable(fieldType):
		fieldTypeText, ok := lowerer.typeName(fieldType)
		if !ok {
			lowerer.typeNameTakenError(call.Pos(), "destructuring default", fieldTypeText)
			return
		}
		zero := zeroValue(fieldType, fieldTypeText)
		if isCompositeZero(fieldType) {
			zero = "(" + zero + ")" // a composite literal in an if header needs parentheses
		}
		condition = tempVar + " != " + zero
	default:
		lowerer.errorf(lowerer.file, call.Pos(), "a destructuring default needs a type that is comparable or can be nil, not %s", displayName(fieldType))
		return
	}
	line := lowerer.line(call.Pos())
	lowerer.replace(call.Pos(), field.Pos(), closureHeader+"if "+tempVar+" := ", line)
	lowerer.replace(field.End(), fallback.Pos(), "; "+condition+" {\nreturn "+result+"\n}\nreturn ", line)
	lowerer.replace(fallback.End(), call.End(), "\n}()", line)
}

func isCompositeZero(goType types.Type) bool {
	if _, ok := goType.(*types.TypeParam); ok {
		return false
	}
	switch goType.Underlying().(type) {
	case *types.Struct, *types.Array:
		return true
	}
	return false
}
