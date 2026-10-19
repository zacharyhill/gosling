package transpile

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/types"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// enumMember is one constant of an enum and the name that the source uses.
type enumMember struct {
	name     string
	constant *types.Const
}

// collectEnums resolves the constants of each enum in the package.
func (transpiler *transpiler) collectEnums() {
	transpiler.enums = map[*types.TypeName][]enumMember{}
	if transpiler.checkedPackage == nil {
		return
	}
	scope := transpiler.checkedPackage.Scope()
	for _, sourceFile := range transpiler.files {
		for _, enum := range sourceFile.preprocessed.Enums {
			enumType, ok := scope.Lookup(enum.Name).(*types.TypeName)
			if !ok {
				continue
			}
			var members []enumMember
			for _, name := range enum.Members {
				if memberConst, ok := scope.Lookup(preprocess.MemberName(enum.Name, name)).(*types.Const); ok {
					members = append(members, enumMember{name: name, constant: memberConst})
				}
			}
			transpiler.enums[enumType] = members
		}
	}
}

// rewriteEnumSelectors replaces each selector like Color.Red, where Color is
// a type with a constant ColorRed of that type, with the constant. It
// reports whether anything changed.
func (transpiler *transpiler) rewriteEnumSelectors(file *file, typeInfo *types.Info) bool {
	changed := false
	astutil.Apply(file.ast, nil, func(cursor *astutil.Cursor) bool {
		selector, ok := cursor.Node().(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if replacement, ok := buildEnumConstant(typeInfo, selector); ok {
			file.enumSelectors[replacement] = selector
			cursor.Replace(replacement)
			changed = true
		}
		return true
	})
	return changed
}

// buildEnumConstant returns the constant that selector names, as an identifier
// or a qualified identifier. Parentheses keep the source range of the
// selector, because lowering uses the end positions of nodes.
func buildEnumConstant(typeInfo *types.Info, selector *ast.SelectorExpr) (*ast.ParenExpr, bool) {
	typeIdent := selector.X
	if qualified, ok := selector.X.(*ast.SelectorExpr); ok {
		typeIdent = qualified.Sel
	}
	ident, ok := typeIdent.(*ast.Ident)
	if !ok {
		return nil, false
	}
	typeName, ok := typeInfo.Uses[ident].(*types.TypeName)
	if !ok || typeName.Pkg() == nil {
		return nil, false
	}
	if object, _, _ := types.LookupFieldOrMethod(typeName.Type(), true, typeName.Pkg(), selector.Sel.Name); object != nil {
		return nil, false
	}
	constName := preprocess.MemberName(typeName.Name(), selector.Sel.Name)
	memberConst, ok := typeName.Pkg().Scope().Lookup(constName).(*types.Const)
	if !ok || !types.Identical(memberConst.Type(), typeName.Type()) {
		return nil, false
	}
	var constExpr ast.Expr = &ast.Ident{Name: constName, NamePos: ident.Pos()}
	if qualified, ok := selector.X.(*ast.SelectorExpr); ok {
		constExpr = &ast.SelectorExpr{X: qualified.X, Sel: constExpr.(*ast.Ident)}
	}
	return &ast.ParenExpr{Lparen: selector.Pos(), X: constExpr, Rparen: selector.End() - 1}, true
}

// lowerEnums writes the String method of each numeric enum, and replaces
// each Color.Red with ColorRed.
func (lowerer *lowerer) lowerEnums() {
	for _, selector := range lowerer.file.enumSelectors {
		typeIdent := selector.X
		if qualified, ok := selector.X.(*ast.SelectorExpr); ok {
			typeIdent = qualified.Sel
		}
		typeName := typeIdent.(*ast.Ident).Name
		memberSuffix := strings.TrimPrefix(preprocess.MemberName(typeName, selector.Sel.Name), typeName)
		if memberSuffix == selector.Sel.Name {
			lowerer.replace(typeIdent.End(), selector.Sel.Pos(), "", 0)
			continue
		}
		lowerer.replace(typeIdent.End(), selector.End(), memberSuffix, 0)
	}
	for _, enum := range lowerer.file.preprocessed.Enums {
		if enum.IsString {
			continue
		}
		start := lowerer.file.tokenFile.Pos(enum.MethodStart)
		strconvName, ok := lowerer.importName(lowerer.file, "strconv", "strconv", false)
		if !ok {
			lowerer.errorf(lowerer.file, start, "enum %s needs package strconv, but the name strconv is already used in this file", enum.Name)
			continue
		}
		method := lowerer.buildStringMethod(enum, qualify(strconvName))
		lowerer.replaceOffsets(enum.MethodStart, enum.MethodEnd, method, lowerer.line(start))
	}
}

// buildStringMethod returns the String method of enum. Each value gives the name
// of its first member.
func (lowerer *lowerer) buildStringMethod(enum preprocess.Enum, strconvPrefix string) string {
	var method strings.Builder
	fmt.Fprintf(&method, "func (v %s) String() string {\nswitch v {\n", enum.Name)
	var seen []constant.Value
	for _, member := range lowerer.lookupEnumMembers(enum.Name) {
		value := member.constant.Val()
		if containsConstant(seen, value) {
			continue
		}
		seen = append(seen, value)
		fmt.Fprintf(&method, "case %s:\nreturn %s\n", member.constant.Name(), strconv.Quote(member.name))
	}
	fmt.Fprintf(&method, "}\nreturn %s + %sItoa(int(v)) + \")\"\n}", strconv.Quote(enum.Name+"("), strconvPrefix)
	return method.String()
}

func (lowerer *lowerer) lookupEnumMembers(name string) []enumMember {
	enumType, _ := lowerer.checkedPackage.Scope().Lookup(name).(*types.TypeName)
	return lowerer.enums[enumType]
}

// checkEnumSwitch reports a switch on an enum that does not list each
// member and has no default case. It skips a switch with a case that is
// not a constant.
func (lowerer *lowerer) checkEnumSwitch(switchStmt *ast.SwitchStmt) {
	if switchStmt.Tag == nil {
		return
	}
	named, ok := types.Unalias(lowerer.typeInfo.Types[switchStmt.Tag].Type).(*types.Named)
	if !ok {
		return
	}
	members, ok := lowerer.enums[named.Obj()]
	if !ok {
		return
	}
	var covered []constant.Value
	for _, clauseStmt := range switchStmt.Body.List {
		clause := clauseStmt.(*ast.CaseClause)
		if clause.List == nil {
			return
		}
		for _, expr := range clause.List {
			value := lowerer.typeInfo.Types[expr].Value
			if value == nil {
				return
			}
			covered = append(covered, value)
		}
	}
	var missing []string
	for _, member := range members {
		if !containsConstant(covered, member.constant.Val()) {
			missing = append(missing, member.constant.Name())
		}
	}
	lowerer.reportMissingCases(switchStmt.Pos(), named.Obj().Name(), missing)
}
