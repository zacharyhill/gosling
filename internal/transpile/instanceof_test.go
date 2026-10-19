package transpile

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestFindNarrowingParts(test *testing.T) {
	testCases := []struct {
		name         string
		body         string
		hasCopy      bool
		wantOk       bool
		wantNarrowed string
	}{
		{"narrowed in the header", "if v, ok := v.(A); ok {\n}", false, true, "v"},
		{"narrowed by the copy", "if a, ok := v.(A); ok {\n\tv := a\n}", true, true, "v"},
		{"no init", "if ok {\n}", false, false, ""},
		{"init without a type assertion", "if v, ok := f(); ok {\n}", false, false, ""},
		{"header target is not a name", "if v.x, ok = v.(A); ok {\n}", false, false, ""},
		{"empty body with a copy", "if a, ok := v.(A); ok {\n}", true, false, ""},
		{"copy target is not a name", "if a, ok := v.(A); ok {\n\tv.x = a\n}", true, false, ""},
		{"first statement is not an assignment", "if a, ok := v.(A); ok {\n\tprintln(a)\n}", true, false, ""},
	}
	for _, testCase := range testCases {
		test.Run(testCase.name, func(test *testing.T) {
			src := "package p\nfunc f() {\n" + testCase.body + "\n}"
			astFile, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
			if err != nil {
				test.Fatal(err)
			}
			funcDecl, isFunc := astFile.Decls[0].(*ast.FuncDecl)
			if !isFunc {
				test.Fatal("want a function declaration")
			}
			ifStmt, isIf := funcDecl.Body.List[0].(*ast.IfStmt)
			if !isIf {
				test.Fatal("want an if statement")
			}
			parts, ok := findNarrowingParts(ifStmt, testCase.hasCopy)
			if ok != testCase.wantOk {
				test.Fatalf("ok = %v, want %v", ok, testCase.wantOk)
			}
			if ok && parts.narrowed.Name != testCase.wantNarrowed {
				test.Errorf("narrowed = %s, want %s", parts.narrowed.Name, testCase.wantNarrowed)
			}
		})
	}
}
