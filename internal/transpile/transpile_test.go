package transpile

import (
	"bytes"
	"flag"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
			name: "optional chaining on a type parameter that allows non-nil types",
			src:  "package p\ntype S interface{ M() int }\nfunc f[P S](v P) int {\n\treturn v?.M()\n}",
			want: "x.gol:4:10: ?. needs a pointer or interface on its left, but type parameter P allows types that cannot be nil",
		},
		{
			name: "nullish coalescing on a type parameter that allows non-nil types",
			src:  "package p\nfunc f[P any](v, d P) P {\n\treturn v ?? d\n}",
			want: "x.gol:3:9: left side of ?? cannot be compared with nil: type parameter P allows types that cannot be nil",
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
			name: "arrow parameter type hidden by a local type",
			src:  "package p\ntype item struct{ Name string }\nvar global = []item{{\"a\"}}\nfunc f() {\n\ttype item struct{ Count int }\n\tprintln(global.map(it => it.Name))\n}",
			want: "x.gol:6:21: the inferred type item cannot be written here, because a local declaration hides the name; rename the local declaration",
		},
		{
			name: "arrow result type hidden by a local variable",
			src:  "package p\ntype item struct{ Name string }\nfunc make2(f func() item) {}\nfunc zero() item { return item{} }\nfunc f() {\n\tlet item = 1\n\t_ = item\n\tmake2(() => zero())\n}",
			want: "x.gol:8:8: the inferred type item cannot be written here, because a local declaration hides the name; rename the local declaration",
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
			name: "fallible call in a ?? default",
			src:  "package p\nimport \"strconv\"\nfunc f(p *int) int {\n\ttry {\n\t\treturn p ?? strconv.Atoi(\"1\")\n\t} catch {\n\t\treturn 0\n\t}\n}",
			want: "x.gol:5:15: this call can fail but only runs conditionally here, so its error cannot be checked; move it into its own statement",
		},
		{
			name: "fallible call in a for post statement",
			src:  "package p\nimport \"strconv\"\nfunc f() {\n\ttry {\n\t\tfor (let i = 0; i < 3; i = strconv.Atoi(\"1\") + 1) {\n\t\t}\n\t} catch {\n\t}\n}",
			want: "x.gol:5:30: this call can fail, but its error cannot be checked here; move it into its own statement",
		},
		{
			name: "fallible call in a later expression of a case",
			src:  "package p\nimport \"strconv\"\nfunc f(n int) {\n\ttry {\n\t\tswitch n {\n\t\tcase 1, strconv.Atoi(\"2\"):\n\t\t}\n\t} catch {\n\t}\n}",
			want: "x.gol:6:11: this call can fail but only runs conditionally here, so its error cannot be checked; move it into its own statement",
		},
		{
			name: "fallthrough into a case with a fallible call",
			src:  "package p\nimport \"strconv\"\nfunc f(n int) {\n\ttry {\n\t\tswitch n {\n\t\tcase 1:\n\t\t\tfallthrough\n\t\tcase strconv.Atoi(\"2\"):\n\t\t}\n\t} catch {\n\t}\n}",
			want: "x.gol:8:3: fallthrough into a case with a call that can fail is not supported",
		},
		{
			name: "default before a case with a fallible call",
			src:  "package p\nimport \"strconv\"\nfunc f(n int) {\n\ttry {\n\t\tswitch n {\n\t\tdefault:\n\t\tcase strconv.Atoi(\"2\"):\n\t\t}\n\t} catch {\n\t}\n}",
			want: "x.gol:7:3: a case with a call that can fail must come before the default case",
		},
		{
			name: "fallible call in switch initializer outside try",
			src:  "package p\nimport \"strconv\"\nfunc f(s string) int {\n\tswitch let n = strconv.Atoi(s); n {\n\tcase 1:\n\t\treturn 1\n\t}\n\treturn 0\n}",
			want: "x.gol:4:17: strconv.Atoi returns an error that is not checked here; assign the error, or move the call into its own statement in a try block",
		},
		{
			name: "fallible call in for initializer in a function literal",
			src:  "package p\nimport \"strconv\"\nfunc f(s string) {\n\ttry {\n\t\tfunc() {\n\t\t\tfor let i = strconv.Atoi(s); i < 3; i++ {\n\t\t\t}\n\t\t}()\n\t} catch {\n\t}\n}",
			want: "x.gol:6:16: strconv.Atoi returns an error that is not checked here; assign the error, or move the call into its own statement in a try block",
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
			name: "type error after annotated parameters",
			src:  "package p\nfunc f(a: int, b: int): string {\n\treturn b\n}",
			want: "x.gol:3:9: cannot use b (variable of type int) as string value in return statement",
		},
		{
			name: "go parameter order with result colon",
			src:  "package p\nfunc f(a int): bool {\n\treturn true\n}",
			want: "x.gol:2:8: write name: type for every parameter",
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

// TestArrowTypeErrorPositions checks errors in the types that gosling fills
// in for an arrow. dep exports functions that use its unexported type.
func TestArrowTypeErrorPositions(test *testing.T) {
	depSrc := "package dep\ntype item struct{}\nfunc Each(f func(item)) {}\nfunc Make(f func() item) {}\nfunc Zero() item { return item{} }\n"
	fileSet := token.NewFileSet()
	depFile, err := parser.ParseFile(fileSet, "dep.go", depSrc, 0)
	if err != nil {
		test.Fatal(err)
	}
	depPackage, err := (&types.Config{}).Check("dep", fileSet, []*ast.File{depFile}, nil)
	if err != nil {
		test.Fatal(err)
	}
	defaultImporter := importer.Default()
	depImporter := importerFunc(func(path string) (*types.Package, error) {
		if path == "dep" {
			return depPackage, nil
		}
		return defaultImporter.Import(path)
	})
	testCases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "parameter type",
			src:  "package p\n\nimport \"dep\"\n\nfunc f() {\n\tdep.Each(it => println(it))\n}\n",
			want: "x.gol:6:11: name item not exported by package dep",
		},
		{
			name: "result type",
			src:  "package p\n\nimport \"dep\"\n\nfunc f() {\n\tdep.Make(() => dep.Zero())\n}\n",
			want: "x.gol:6:11: name item not exported by package dep",
		},
	}
	for _, testCase := range testCases {
		test.Run(testCase.name, func(test *testing.T) {
			_, err := Transpile(Config{Importer: depImporter}, []Input{{Path: "x.gol", Src: []byte(testCase.src)}})
			if err == nil {
				test.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				test.Errorf("error:\n%v\nwant substring:\n%s", err, testCase.want)
			}
		})
	}
}

type importerFunc func(path string) (*types.Package, error)

func (importFunc importerFunc) Import(path string) (*types.Package, error) { return importFunc(path) }

// TestUncheckedAssignsInSourceOrder reads the lowering errors before
// Transpile sorts them, so it catches an order that changes between runs.
func TestUncheckedAssignsInSourceOrder(test *testing.T) {
	src := "package p\nimport \"strconv\"\nfunc f(s string) {\n" +
		"\tlet a = strconv.Atoi(s)\n\tlet b = strconv.Atoi(s)\n\tlet c = strconv.Atoi(s)\n" +
		"\tlet d = strconv.Atoi(s)\n\tlet e = strconv.Atoi(s)\n\tlet g = strconv.Atoi(s)\n" +
		"\tprintln(a, b, c, d, e, g)\n}"
	session := &transpiler{config: Config{Importer: importer.Default()}, fileSet: token.NewFileSet()}
	if err := session.parse([]Input{{Path: "x.gol", Src: []byte(src)}}); err != nil {
		test.Fatal(err)
	}
	typeInfo := session.check()
	if err := session.diagnostics.Err(); err != nil {
		test.Fatal(err)
	}
	newLowerer(session, session.files[0], typeInfo).lowerFile()
	var gotLines []int
	for _, diagnostic := range session.diagnostics {
		gotLines = append(gotLines, diagnostic.Pos.Line)
	}
	wantLines := []int{4, 5, 6, 7, 8, 9}
	if !slices.Equal(gotLines, wantLines) {
		test.Errorf("error lines = %v, want %v\n%v", gotLines, wantLines, session.diagnostics)
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

// TestPackageScopeNames checks that generated names do not shadow names
// that another file of the package declares.
func TestPackageScopeNames(test *testing.T) {
	declarations := `package p

type ret struct{ code int }

type v struct{ code int }

type err struct{ code int }

type box struct{}

func each(visit func(ret)) { visit(ret{1}) }

func eachFailure(visit func(err)) { visit(err{3}) }

func (b *box) each(visit func(v)) { visit(v{2}) }
`
	uses := `package p

import "os"

func count() int {
	try {
		os.Remove("a")
		each(item => println(item.code))
		return 1
	} catch {
		return 0
	} finally {
		println("done")
	}
}

func visit(b *box) {
	b?.each(item => println(item.code))
}

func size(path string) int {
	try {
		const contents = os.ReadFile(path)
		eachFailure(item => println(item.code))
		return len(contents)
	} catch {
		return 0
	}
}
`
	inputs := []Input{{Path: "declarations.gol", Src: []byte(declarations)}, {Path: "uses.gol", Src: []byte(uses)}}
	outputs, err := Transpile(Config{Importer: importer.Default()}, inputs)
	if err != nil {
		test.Fatal(err)
	}
	fileSet := token.NewFileSet()
	var parsedFiles []*ast.File
	for _, output := range outputs {
		parsedFile, err := parser.ParseFile(fileSet, output.Path, output.Src, 0)
		if err != nil {
			test.Fatal(err)
		}
		parsedFiles = append(parsedFiles, parsedFile)
	}
	var typeErrors []string
	checkConfig := types.Config{Importer: importer.Default(), Error: func(err error) { typeErrors = append(typeErrors, err.Error()) }}
	if _, err := checkConfig.Check("p", fileSet, parsedFiles, nil); err != nil {
		test.Fatalf("generated code does not type-check:\n%s\n%s", strings.Join(typeErrors, "\n"), outputs[1].Src)
	}
}

// TestImportPackageNames checks that an added import gets an alias if
// another file of the package declares its name.
func TestImportPackageNames(test *testing.T) {
	declarations := `package p

import clock "time"

type fmt struct{}

type strconv struct{}

type slices struct{}

type time struct{}

func wait(visit func(clock.Duration)) { visit(clock.Second) }
`
	uses := `package p

func describe(count int, ratio float64, first, second []int) []int {
	println(t` + "`count=${count} ratio=${ratio}`" + `)
	wait(duration => println(duration.String()))
	return [...first, ...second]
}
`
	inputs := []Input{{Path: "declarations.gol", Src: []byte(declarations)}, {Path: "uses.gol", Src: []byte(uses)}}
	outputs, err := Transpile(Config{Importer: importer.Default()}, inputs)
	if err != nil {
		test.Fatal(err)
	}
	fileSet := token.NewFileSet()
	var parsedFiles []*ast.File
	for _, output := range outputs {
		parsedFile, err := parser.ParseFile(fileSet, output.Path, output.Src, 0)
		if err != nil {
			test.Fatal(err)
		}
		parsedFiles = append(parsedFiles, parsedFile)
	}
	var typeErrors []string
	checkConfig := types.Config{Importer: importer.Default(), Error: func(err error) { typeErrors = append(typeErrors, err.Error()) }}
	if _, err := checkConfig.Check("p", fileSet, parsedFiles, nil); err != nil {
		test.Fatalf("generated code does not type-check:\n%s\n%s", strings.Join(typeErrors, "\n"), outputs[1].Src)
	}
	for _, want := range []string{`fmtpkg "fmt"`, `strconvpkg "strconv"`, `slicespkg "slices"`, `timepkg "time"`} {
		if !strings.Contains(string(outputs[1].Src), want) {
			test.Errorf("want import %s in:\n%s", want, outputs[1].Src)
		}
	}
}

// TestLineDirectiveAfterComma checks the lines of tokens after a comma that
// generated text puts on an earlier line. gofmt moves a directive between
// the comma and the token in front of the comma.
func TestLineDirectiveAfterComma(test *testing.T) {
	src := "package p\n\nfunc pick(n int) int { return n }\n\nfunc show() []string {\n" +
		"\treturn []string{t`one ${pick(1)}\ntwo ${pick(2)}`, t`three\n${pick(3)}`,\n\t\tt`${pick(4)}`}\n}\n"
	outputs, err := Transpile(Config{Importer: importer.Default(), LineDirectives: true}, []Input{{Path: "p.gol", Src: []byte(src)}})
	if err != nil {
		test.Fatal(err)
	}
	fileSet := token.NewFileSet()
	parsedFile, err := parser.ParseFile(fileSet, "p.go", outputs[0].Src, parser.ParseComments)
	if err != nil {
		test.Fatal(err)
	}
	wantLines := map[string]int{"1": 6, "2": 7, "3": 8, "4": 9}
	ast.Inspect(parsedFile, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); !ok || ident.Name != "pick" {
			return true
		}
		argument := call.Args[0].(*ast.BasicLit).Value
		if got := fileSet.PositionFor(call.Pos(), true).Line; got != wantLines[argument] {
			test.Errorf("pick(%s) maps to line %d, want %d\n%s", argument, got, wantLines[argument], outputs[0].Src)
		}
		return true
	})
}

// TestLineDirectiveHoistedCall checks that a call that try moves before its
// statement keeps its own line.
func TestLineDirectiveHoistedCall(test *testing.T) {
	src := "package p\n\nimport \"strconv\"\n\nfunc sum(a, b string) (int, error) {\n\ttry {\n\t\treturn max(\n" +
		"\t\t\tstrconv.Atoi(a),\n\t\t\tstrconv.Atoi(b),\n\t\t), nil\n\t} catch (e) {\n\t\treturn 0, e\n\t}\n}\n"
	outputs, err := Transpile(Config{Importer: importer.Default(), LineDirectives: true}, []Input{{Path: "p.gol", Src: []byte(src)}})
	if err != nil {
		test.Fatal(err)
	}
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "p.go", outputs[0].Src, parser.ParseComments)
	if err != nil {
		test.Fatal(err)
	}
	wantLines := map[string]int{"a": 8, "b": 9}
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Atoi" {
			return true
		}
		argumentName := call.Args[0].(*ast.Ident).Name
		if got := fileSet.PositionFor(call.Pos(), true).Line; got != wantLines[argumentName] {
			test.Errorf("strconv.Atoi(%s) maps to line %d, want %d\n%s", argumentName, got, wantLines[argumentName], outputs[0].Src)
		}
		return true
	})
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
