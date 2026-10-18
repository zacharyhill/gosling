package transpile

import (
	"bytes"
	"flag"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

func transpileFile(test *testing.T, path string, lineDirectives bool) []byte {
	test.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		test.Fatal(err)
	}
	outputs, err := Transpile(Config{Importer: importer.Default(), LineDirectives: lineDirectives}, []Input{{Path: path, Src: src}})
	if err != nil {
		test.Fatalf("Transpile: %v", err)
	}
	return outputs[0].Src
}

func TestGolden(test *testing.T) {
	paths, err := filepath.Glob("testdata/golden/*.gol")
	if err != nil {
		test.Fatal(err)
	}
	for _, path := range paths {
		test.Run(filepath.Base(path), func(test *testing.T) {
			got := transpileFile(test, path, strings.HasPrefix(filepath.Base(path), "lines"))
			goldenPath := strings.TrimSuffix(path, ".gol") + ".go.golden"
			if *updateGolden {
				writeFile(test, goldenPath, string(got))
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				test.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				test.Errorf("output differs from %s; rerun with -update and review the diff\n%s", goldenPath, got)
			}
			formatted, err := format.Source(got)
			if err != nil {
				test.Fatal(err)
			}
			if !bytes.Equal(formatted, got) {
				test.Errorf("output is not gofmt-stable\n%s", formatted)
			}
		})
	}
}

// TestGoldenCompiles vets each golden file, so the expected output is valid
// Go and not only stable.
func TestGoldenCompiles(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	paths, err := filepath.Glob("testdata/golden/*.go.golden")
	if err != nil {
		test.Fatal(err)
	}
	directory := test.TempDir()
	writeFile(test, filepath.Join(directory, "go.mod"), "module golden\n\ngo 1.26\n")
	for _, path := range paths {
		packageName := strings.TrimSuffix(filepath.Base(path), ".go.golden")
		if err := os.Mkdir(filepath.Join(directory, packageName), 0o755); err != nil {
			test.Fatal(err)
		}
		writeFile(test, filepath.Join(directory, packageName, packageName+".go"), mustRead(test, path))
	}
	vetCommand := exec.Command("go", "vet", "./...")
	vetCommand.Dir = directory
	if output, err := vetCommand.CombinedOutput(); err != nil {
		test.Fatalf("go vet: %v\n%s", err, output)
	}
}

func TestErrors(test *testing.T) {
	testCases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "never-returning calls only end a function through try",
			src:  "package p\nimport \"os\"\nfunc f(ok bool) int {\n\tif ok {\n\t\treturn 1\n\t} else {\n\t\tos.Exit(1)\n\t}\n}",
			want: "x.gol:9:1: missing return",
		},
		{
			name: "a try whose catch can complete still needs a return",
			src:  "package p\nimport \"os\"\nfunc f(p string) []byte {\n\ttry {\n\t\treturn os.ReadFile(p)\n\t} catch (e) {\n\t\tprintln(e)\n\t}\n}",
			want: "x.gol:9:1: missing return",
		},
		{
			name: "bare call with error before the last result inside try",
			src:  "package p\nimport \"errors\"\nfunc b() (error, int) { return errors.New(\"x\"), 0 }\nfunc f() {\n\ttry {\n\t\tb()\n\t} catch (e) {\n\t\tprintln(e)\n\t}\n}",
			want: "x.gol:6:3: error is not the last result of b, so try cannot check it; assign the results and check the error explicitly",
		},
		{
			name: "question mark operator",
			src:  "package p\nimport \"os\"\nfunc f() error {\n\tos.Remove(\"x\")?\n\treturn nil\n}",
			want: "x.gol:4:16: the ? operator is not supported; use try/catch",
		},
		{
			name: "type error positions map to source",
			src:  "package p\nfunc f() {\n\ttry { undefinedThing() } catch (e) {}\n}",
			want: "x.gol:3:8: undefined: undefinedThing",
		},
		{
			name: "template literal needs fmt but the name is taken",
			src:  "package p\nfunc f(fmt int) string {\n\treturn t`${fmt}`\n}",
			want: "x.gol:3:9: template literal needs package fmt, but the name fmt is already used in this file",
		},
		{
			name: "assignment to a const variable",
			src:  "package p\nfunc f(n int) int {\n\tconst x = n * 2\n\tx = 3\n\treturn x\n}",
			want: "x.gol:4:2: cannot assign to x: declared with const",
		},
		{
			name: "increment of a const variable",
			src:  "package p\nfunc f(n int) int {\n\tconst x = n\n\tx++\n\treturn x\n}",
			want: "x.gol:4:2: cannot assign to x: declared with const",
		},
		{
			name: "assignment to a package-level const variable",
			src:  "package p\nimport \"os\"\nconst home = os.Getenv(\"HOME\")\nfunc f() {\n\thome, _ = \"x\", 1\n}",
			want: "x.gol:5:2: cannot assign to home: declared with const",
		},
		{
			name: "assignment to a const for-of variable",
			src:  "package p\nfunc f(xs []int) {\n\tfor (const x of xs) {\n\t\tx = 1\n\t\t_ = x\n\t}\n}",
			want: "x.gol:4:3: cannot assign to x: declared with const",
		},
		{
			name: "assignment to a const destructured variable",
			src:  "package p\ntype P struct{ X int }\nfunc f(p P) int {\n\tconst {X} = p\n\tX++\n\treturn X\n}",
			want: "x.gol:5:2: cannot assign to X: declared with const",
		},
		{
			name: "assignment to a const destructured call result",
			src:  "package p\nfunc g() []int { return nil }\nfunc f() int {\n\tconst [a, b] = g()\n\tb = a\n\treturn b\n}",
			want: "x.gol:5:2: cannot assign to b: declared with const",
		},
		{
			name: "assignment to an if const",
			src:  "package p\nfunc g() int { return 1 }\nfunc f() {\n\tif const x = g(); x > 0 {\n\t\tx = 2\n\t}\n}",
			want: "x.gol:5:3: cannot assign to x: declared with const",
		},
		{
			name: "increment of a c-style for const",
			src:  "package p\nfunc f() {\n\tfor (const i = 0; i < 3; i++) {\n\t}\n}",
			want: "x.gol:3:27: cannot assign to i: declared with const",
		},
		{
			name: "assignment to a const type switch variable",
			src:  "package p\nfunc f(x any) {\n\tswitch const v = x.(type) {\n\tcase int:\n\t\tv = 2\n\t\t_ = v\n\t}\n}",
			want: "x.gol:5:3: cannot assign to v: declared with const",
		},
		{
			name: "short variable declaration",
			src:  "package p\nfunc f() int {\n\tx := 1\n\treturn x\n}",
			want: "x.gol:3:4: use let or const instead of :=",
		},
		{
			name: "short variable declaration in a header",
			src:  "package p\nfunc f() {\n\tfor i := 0; i < 3; i++ {\n\t}\n}",
			want: "x.gol:3:8: use let or const instead of :=",
		},
		{
			name: "var in a function",
			src:  "package p\nvar top int\nfunc f() int {\n\tvar x int\n\treturn x + top\n}",
			want: "x.gol:4:2: use let instead of var in a function",
		},
		{
			name: "var in a for-of loop",
			src:  "package p\nfunc f(xs []int) {\n\tfor (var x of xs) {\n\t\t_ = x\n\t}\n}",
			want: "x.gol:3:7: use let instead of var in a function",
		},
		{
			name: "assignment to a Go constant",
			src:  "package p\nfunc f() {\n\tconst x = 1\n\tx = 2\n}",
			want: "x.gol:4:2: cannot assign to x",
		},
		{
			name: "destructuring unknown field",
			src:  "package p\ntype P struct{ X int }\nfunc f(p P) int {\n\tlet {Z} = p\n\treturn Z\n}",
			want: "x.gol:4:6: p.Z undefined",
		},
		{
			name: "comma-ok destructuring of a slice index",
			src:  "package p\nfunc f(rows [][]int) int {\n\tconst [a, b] = rows[0]\n\treturn a + b\n}",
			want: "x.gol:3:17: rows[0] is not a map index, a type assertion, or a receive; to destructure the value, assign it to a variable first",
		},
		{
			name: "nullish coalescing on a value that is never nil",
			src:  "package p\nfunc f(n int) int {\n\treturn n ?? 1\n}",
			want: "x.gol:3:9: left side of ?? is never nil",
		},
		{
			name: "optional chaining on a struct value",
			src:  "package p\ntype P struct{ X int }\nfunc f(p P) int {\n\treturn p?.X\n}",
			want: "x.gol:4:10: ?. needs a pointer or interface on its left, not P",
		},
		{
			name: "nullish coalescing mixed with ||",
			src:  "package p\nfunc f(a *bool, b bool) bool {\n\treturn a ?? b || true\n}",
			want: "x.gol:3:9: ?? cannot be mixed with || or && without parentheses",
		},
		{
			name: "optional call returning error in try",
			src:  "package p\nimport \"io\"\nfunc f(c io.Closer) {\n\ttry {\n\t\tc?.Close()\n\t\tc.Close()\n\t} catch {\n\t}\n}",
			want: "x.gol:5:3: the error from an optional call is not checked inside try; use an explicit nil check",
		},
		{
			name: "arrow without parameter types or context",
			src:  "package p\nfunc f() {\n\tlet g = x => x\n\t_ = g\n}",
			want: "x.gol:3:10: cannot infer the parameter types of this arrow function; add types, as in (x int) => ...",
		},
		{
			name: "forEach used as a value",
			src:  "package p\nfunc f(xs []int) {\n\tlet y = xs.forEach(x => println(x))\n\t_ = y\n}",
			want: "x.gol:3:10: forEach can only be used as a statement",
		},
		{
			name: "switch missing union variants",
			src:  "package p\ntype A struct{}\ntype B struct{}\ntype C struct{}\ntype U = A | B | C\nfunc f(u U) {\n\tswitch u.(type) {\n\tcase A:\n\t}\n}",
			want: "x.gol:7:2: switch on U is missing cases: B, C",
		},
		{
			name: "union variant from another package",
			src:  "package p\nimport \"time\"\ntype A struct{}\ntype U = A | time.Time\n",
			want: "x.gol:4:1: union variant time.Time must be a type declared in this package",
		},
		{
			name: "union variant that is an interface",
			src:  "package p\ntype A struct{}\ntype I interface{ M() }\ntype U = A | I\n",
			want: "x.gol:4:1: union variant I must not be an interface",
		},
		{
			name: "fallible call after &&",
			src:  "package p\nimport \"strconv\"\nfunc f(ok bool) bool {\n\ttry {\n\t\treturn ok && strconv.Atoi(\"1\") > 0\n\t} catch {\n\t\treturn false\n\t}\n}",
			want: "x.gol:5:16: this call can fail but only runs conditionally here, so its error cannot be checked; move it into its own statement",
		},
		{
			name: "fallible call in loop condition",
			src:  "package p\nimport \"strconv\"\nfunc f() {\n\ttry {\n\t\tfor strconv.Atoi(\"1\") > 0 {\n\t\t}\n\t} catch {\n\t}\n}",
			want: "x.gol:5:7: this call can fail, but its error cannot be checked here; move it into its own statement",
		},
		{
			name: "positional field in struct spread",
			src:  "package p\ntype P struct{ X, Y int }\nfunc f(p P) P {\n\treturn P{...p, 1}\n}",
			want: "x.gol:4:17: fields after a struct spread must be named",
		},
		{
			name: "missing catch",
			src:  "package p\nfunc f() {\n\ttry { }\n}",
			want: "x.gol:3:8: try block must be followed by catch",
		},
		{
			name: "parse error positions map to source",
			src:  "package p\nfunc f() {\n\ttry { let x = } catch (e) {}\n}",
			want: "x.gol:3:16: expected operand",
		},
	}
	for _, testCase := range testCases {
		test.Run(testCase.name, func(test *testing.T) {
			_, err := Transpile(Config{Importer: importer.Default()}, []Input{{Path: "x.gol", Src: []byte(testCase.src)}})
			if err == nil {
				test.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				test.Errorf("error:\n%v\nwant substring:\n%s", err, testCase.want)
			}
		})
	}
}

// TestRun builds and runs each program in testdata/run. It compares the
// output with the matching .out file.
func TestRun(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	paths, err := filepath.Glob("testdata/run/*.gol")
	if err != nil {
		test.Fatal(err)
	}
	for _, path := range paths {
		test.Run(filepath.Base(path), func(test *testing.T) {
			directory := test.TempDir()
			generated := transpileFile(test, path, true)
			writeFile(test, filepath.Join(directory, "go.mod"), "module run\n\ngo 1.26\n")
			writeFile(test, filepath.Join(directory, filepath.Base(path)), mustRead(test, path))
			writeFile(test, filepath.Join(directory, "main.go"), string(generated))
			runCommand := exec.Command("go", "run", ".")
			runCommand.Dir = directory
			got, err := runCommand.CombinedOutput()
			if err != nil {
				test.Fatalf("go run: %v\n%s\ngenerated:\n%s", err, got, generated)
			}
			want := mustRead(test, strings.TrimSuffix(path, ".gol")+".out")
			if string(got) != want {
				test.Errorf("output:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// TestLineDirectives checks that stack traces from code after a lowered try
// block point at the original .gol line.
func TestLineDirectives(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	src := `// Package main has a doc comment, which gofmt rearranges around directives.
package main

import "os"

func main() {
	try {
		os.Remove("a")
		os.Remove("b")
	} catch (e) {
		println(e.Error())
	}
	let msg = t` + "`two\nlines ${len(os.Args)}`" + `
	let [first] = os.Args[:1]
	let {Name: name} = struct{ Name string }{first + msg}
	panic(name)
}
`
	outputs, err := Transpile(Config{Importer: importer.Default(), LineDirectives: true}, []Input{{Path: "prog.gol", Src: []byte(src)}})
	if err != nil {
		test.Fatal(err)
	}
	directory := test.TempDir()
	writeFile(test, filepath.Join(directory, "go.mod"), "module run\n\ngo 1.26\n")
	writeFile(test, filepath.Join(directory, "prog.go"), string(outputs[0].Src))
	runCommand := exec.Command("go", "run", ".")
	runCommand.Dir = directory
	got, err := runCommand.CombinedOutput()
	if err == nil {
		test.Fatalf("expected panic, got none:\n%s", outputs[0].Src)
	}
	if !strings.Contains(string(got), "prog.gol:17") {
		test.Errorf("stack trace does not point at prog.gol:17:\n%s\ngenerated:\n%s", got, outputs[0].Src)
	}
}

// TestLineDirectivePositions checks that each function in the output maps
// to its .gol line. Lowered code that ends a block must not shift later
// lines.
func TestLineDirectivePositions(test *testing.T) {
	src := `package p

import "os"

func double(list []int) []int {
	return list.map(x => x * 2)
}

func name(nickname *string) string {
	if len(os.Args) > 0 {
		return nickname ?? "anon"
	}
	return ""
}

func remove(path string) error {
	try {
		os.Remove(path)
	} catch (failure) {
		return failure
	}
	return nil
}

type box struct {
	inner *box
	size  int
}

func sizes(outer *box) []int {
	return []int{
		outer?.inner?.size,
		outer.size,
	}
}

func pick(choice int, err error) string {
	switch choice {
	case 1:
		try {
			throws(err)
		} catch (failure) {
			return failure.Error()
		}
	default:
		return "default"
	}
	return ` + "t`choice=${choice}`" + `
}

func throws(err error) error { return err }

func last() int {
	return 1
}
`
	outputs, err := Transpile(Config{Importer: importer.Default(), LineDirectives: true}, []Input{{Path: "p.gol", Src: []byte(src)}})
	if err != nil {
		test.Fatal(err)
	}
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "p.go", outputs[0].Src, parser.ParseComments)
	if err != nil {
		test.Fatal(err)
	}
	lines := strings.Split(src, "\n")
	for _, decl := range parsed.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		wantLine := 0
		for lineIndex, line := range lines {
			if strings.HasPrefix(line, "func "+funcDecl.Name.Name+"(") {
				wantLine = lineIndex + 1
			}
		}
		if got := fileSet.PositionFor(funcDecl.Name.Pos(), true).Line; got != wantLine {
			test.Errorf("func %s maps to line %d, want %d\n%s", funcDecl.Name.Name, got, wantLine, outputs[0].Src)
		}
	}
}

func writeFile(test *testing.T, path, content string) {
	test.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		test.Fatal(err)
	}
}

func mustRead(test *testing.T, path string) string {
	test.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		test.Fatal(err)
	}
	return string(contents)
}

func TestKeepGoingEmitsDespiteTypeErrors(test *testing.T) {
	src := "package p\n\nimport \"fmt\"\n\nfunc f() {\n\tfmt.Pr\n}\n\nfunc g() {\n\tconst n = 1\n\tfmt.Println(t`${n}`)\n}\n"
	inputs := []Input{{Path: "x.gol", Src: []byte(src)}}
	outputs, err := Transpile(Config{Importer: importer.Default(), KeepGoing: true}, inputs)
	if err == nil {
		test.Fatal("want the type error")
	}
	if !strings.Contains(err.Error(), "Pr") {
		test.Errorf("error = %v, want the error for fmt.Pr", err)
	}
	if len(outputs) != 1 {
		test.Fatalf("got %d outputs, want 1", len(outputs))
	}
	if generated := string(outputs[0].Src); !strings.Contains(generated, "fmt.Pr\n") || !strings.Contains(generated, "fmt.Sprint") {
		test.Errorf("want the lowered file with fmt.Pr kept, got:\n%s", generated)
	}
	if outputs, err := Transpile(Config{Importer: importer.Default()}, inputs); err == nil || outputs != nil {
		test.Errorf("without KeepGoing got %d outputs, %v, want no outputs and an error", len(outputs), err)
	}
}

func TestIsGeneratedFile(test *testing.T) {
	directory := test.TempDir()
	files := map[string]string{
		"generated.go": GeneratedPrefix + " from x.gol. DO NOT EDIT.\n\npackage x\n",
		"written.go":   "package x\n",
		"short.go":     "//",
	}
	want := map[string]bool{"generated.go": true, "written.go": false, "short.go": false}
	for name, content := range files {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			test.Fatal(err)
		}
		isGenerated, err := IsGeneratedFile(path)
		if err != nil || isGenerated != want[name] {
			test.Errorf("IsGeneratedFile(%s) = %v, %v, want %v", name, isGenerated, err, want[name])
		}
	}
	if _, err := IsGeneratedFile(filepath.Join(directory, "missing.go")); err == nil {
		test.Error("want an error for a missing file")
	}
}

func TestIsGenerated(test *testing.T) {
	if !IsGenerated([]byte(GeneratedPrefix + " from x.gol. DO NOT EDIT.\n")) {
		test.Error("want true for a file with the gosling header")
	}
	if IsGenerated([]byte("package x\n")) || IsGenerated([]byte("//")) {
		test.Error("want false for a file without the gosling header")
	}
}
