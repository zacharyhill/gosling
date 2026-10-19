package transpile

import (
	"go/ast"
	"go/token"
	"strconv"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// regexMarkerDecl declares the regex marker for type checking. The markers
// file then imports regexp.
const regexMarkerDecl = "func " + preprocess.RegexMarker + "(pattern string) *regexp.Regexp { return nil }\n"

func (transpiler *transpiler) usesRegex() bool {
	for _, file := range transpiler.files {
		if file.hasRegex {
			return true
		}
	}
	return false
}

// lowerRegexes writes each regex literal as a regexp.MustCompile call. In a
// function, the call moves to a new package-level variable before the
// declaration, so the pattern compiles one time.
func (lowerer *lowerer) lowerRegexes() {
	if !lowerer.file.hasRegex {
		return
	}
	regexpName, ok := lowerer.importName(lowerer.file, "regexp", "regexp", false)
	if !ok {
		lowerer.errorf(lowerer.file, lowerer.file.ast.Name.Pos(), "regex literal needs package regexp, but the name regexp is already used in this file")
		return
	}
	compileName := qualify(regexpName) + "MustCompile"
	for _, decl := range lowerer.file.ast.Decls {
		_, isFunc := decl.(*ast.FuncDecl)
		lowerer.lowerRegexesIn(decl, decl, isFunc, compileName)
	}
}

func (lowerer *lowerer) lowerRegexesIn(decl ast.Decl, root ast.Node, inFunction bool, compileName string) {
	ast.Inspect(root, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncLit:
			if node != root {
				lowerer.lowerRegexesIn(decl, node, true, compileName)
				return false
			}
		case *ast.CallExpr:
			callee, ok := node.Fun.(*ast.Ident)
			if !ok || callee.Name != preprocess.RegexMarker {
				return true
			}
			if inFunction {
				lowerer.hoistRegex(decl, node, compileName)
			} else {
				lowerer.replace(callee.Pos(), callee.End(), compileName, lowerer.line(callee.Pos()))
			}
		}
		return true
	})
}

// hoistRegex declares a package-level variable for the regex marker call
// before decl, and uses the variable in place of the call.
func (lowerer *lowerer) hoistRegex(decl ast.Decl, call *ast.CallExpr, compileName string) {
	name := lowerer.pickPackageName("re")
	line := lowerer.line(call.Pos())
	declStart := decl.Pos()
	if doc := getDocComment(decl); doc != nil {
		declStart = doc.Pos()
	}
	pattern := lowerer.file.text(call.Args[0])
	lowerer.insert(declStart, "var "+name+" = "+compileName+"("+pattern+")\n\n", line)
	lowerer.replace(call.Pos(), call.End(), name, line)
}

func getDocComment(decl ast.Decl) *ast.CommentGroup {
	switch decl := decl.(type) {
	case *ast.FuncDecl:
		return decl.Doc
	case *ast.GenDecl:
		return decl.Doc
	}
	return nil
}

// pickPackageName returns base followed by a number. The name is not used
// in any .gol file of the package or at package level.
func (lowerer *lowerer) pickPackageName(base string) string {
	for {
		lowerer.packageNameCount++
		name := base + strconv.Itoa(lowerer.packageNameCount)
		if !lowerer.isUsedInPackage(name) {
			lowerer.identPositions[name] = append(lowerer.identPositions[name], token.NoPos)
			return name
		}
	}
}

func (transpiler *transpiler) isUsedInPackage(name string) bool {
	if transpiler.checkedPackage != nil && transpiler.checkedPackage.Scope().Lookup(name) != nil {
		return true
	}
	for _, file := range transpiler.files {
		if file.identNames[name] {
			return true
		}
	}
	return false
}
