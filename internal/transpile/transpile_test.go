package transpile

import (
	"bytes"
	"errors"
	"flag"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
			name: "checked call in a ternary arm",
			src:  "package p\nimport \"strconv\"\nfunc f(ok bool, s string) int {\n\ttry {\n\t\treturn ok ? strconv.Atoi(s) : 0\n\t} catch {\n\t\treturn -1\n\t}\n}",
			want: "x.gol:5:15: " + errConditionalCall,
		},
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
			name: "ternary arms with mismatched types",
			src:  "package p\nfunc f(c bool) {\n\tlet x = c ? 1 : \"a\"\n\t_ = x\n}",
			want: "x.gol:3:18: ternary arms have mismatched types untyped int and untyped string",
		},
		{
			name: "ternary with a condition that is not a bool",
			src:  "package p\nfunc f(n int) int {\n\treturn n ? 1 : 2\n}",
			want: "x.gol:3:9: cannot use n (variable of type int) as bool value in ternary",
		},
		{
			name: "ternary with unrelated arm types",
			src:  "package p\nfunc f(c bool, n int, s string) {\n\tlet x = c ? n : s\n\t_ = x\n}",
			want: "x.gol:3:18: ternary arms have mismatched types int and string",
		},
		{
			name: "ternary with nil arms and no context",
			src:  "package p\nfunc f(c bool) {\n\tlet x = c ? nil : nil\n\t_ = x\n}",
			want: "x.gol:3:10: cannot infer the type of this ternary",
		},
		{
			name: "fallible call in a ternary arm",
			src:  "package p\nimport \"strconv\"\nfunc f(s string) int {\n\ttry {\n\t\treturn s == \"\" ? 0 : strconv.Atoi(s)\n\t} catch {\n\t\treturn -1\n\t}\n}",
			want: "x.gol:5:24: this call can fail but only runs conditionally here",
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
			name: "template literal float needs strconv but the name is taken",
			src:  "package p\nfunc f(strconv int, x float64) string {\n\treturn t`${x}`\n}",
			want: "x.gol:3:13: template literal needs package strconv, but the name strconv is already used in this file",
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
			name: "assignment to a const for-of variable over a channel",
			src:  "package p\nfunc f(ch chan int) {\n\tfor (const x of ch) {\n\t\tx = 1\n\t\t_ = x\n\t}\n}",
			want: "x.gol:4:3: cannot assign to x: declared with const",
		},
		{
			name: "assignment to a const for-of variable over an integer",
			src:  "package p\nfunc f(n int) {\n\tfor (const i of n) {\n\t\ti++\n\t}\n}",
			want: "x.gol:4:3: cannot assign to i: declared with const",
		},
		{
			name: "assignment to a const for-of destructured variable",
			src:  "package p\ntype P struct{ X int }\nfunc f(ps []P) {\n\tfor (const {X} of ps) {\n\t\tX = 1\n\t\t_ = X\n\t}\n}",
			want: "x.gol:5:3: cannot assign to X: declared with const",
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
			name: "assignment to a const nested destructured variable",
			src:  "package p\ntype P struct{ X, Y int }\nfunc f(ps [][]P) int {\n\tconst [[_, {Y}]] = ps\n\tY = 2\n\treturn Y\n}",
			want: "x.gol:5:2: cannot assign to Y: declared with const",
		},
		{
			name: "assignment to a const destructured tuple",
			src:  "package p\nfunc g() (int, int) { return 1, 2 }\nfunc f() int {\n\tconst [a, b] = g()\n\tb = a\n\treturn b\n}",
			want: "x.gol:5:2: cannot assign to b: declared with const",
		},
		{
			name: "array pattern with fewer names than results",
			src:  "package p\nfunc g() (int, int, int) { return 1, 2, 3 }\nfunc f() int {\n\tconst [a, b] = g()\n\treturn a + b\n}",
			want: "x.gol:4:17: assignment mismatch: 2 variables but g returns 3 values",
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
			name: "call through a function value cannot omit defaults",
			src:  "package p\nfunc f(a int, b int = 1) {}\nfunc g() {\n\tconst h = f\n\th(1)\n}",
			want: "x.gol:5:5: not enough arguments in call to h",
		},
		{
			name: "call through an interface cannot omit defaults",
			src:  "package p\ntype T struct{}\nfunc (T) M(a int = 1) {}\ntype I interface{ M(a int) }\nfunc g(i I) {\n\ti.M()\n}",
			want: "x.gol:6:6: not enough arguments in call to i.M",
		},
		{
			name: "call without a required argument",
			src:  "package p\nfunc f(a int, b int = 1) {}\nfunc g() {\n\tf()\n}",
			want: "x.gol:4:4: not enough arguments in call to f",
		},
		{
			name: "default value of the wrong type",
			src:  "package p\nfunc f(a int = \"x\") {}",
			want: "x.gol:2:16: cannot use \"x\" (untyped string constant) as int value in default value",
		},
		{
			name: "default value uses another parameter",
			src:  "package p\nfunc f(a int, b int = a + 1) {}",
			want: "x.gol:2:23: a default value cannot use the parameter a",
		},
		{
			name: "default value in an interface method",
			src:  "package p\ntype I interface {\n\tM(a int = 1)\n}",
			want: "x.gol:3:10: default values are only supported in function and method declarations",
		},
		{
			name: "default value in a generic function",
			src:  "package p\nfunc f[T any](x T, n int = 1) {}",
			want: "x.gol:2:20: default values are not supported in generic functions",
		},
		{
			name: "default value in a function literal",
			src:  "package p\nvar f = func(a int = 1) {}",
			want: "x.gol:2:20: default values are only supported in function and method declarations",
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
			name: "typed header with a value of another type",
			src:  "package p\nfunc f(n int) {\n\tfor (let i: int64 = n; i < 3; i++) {\n\t}\n}",
			want: "x.gol:3:22: cannot use n (variable of type int) as int64 value in variable declaration",
		},
		{
			name: "typed header with a rune constant for a string",
			src:  "package p\nfunc f() {\n\tif let s: string = 'a'; s != \"\" {\n\t}\n}",
			want: "x.gol:3:21: cannot use 'a' (untyped rune constant 97) as string value in variable declaration",
		},
		{
			name: "typed header with a string constant for a byte slice",
			src:  "package p\nfunc f() {\n\tif let b: []byte = \"a\"; len(b) > 0 {\n\t}\n}",
			want: "x.gol:3:21: cannot use \"a\" (untyped string constant) as []byte value in variable declaration",
		},
		{
			name: "typed header with a constant that overflows",
			src:  "package p\nfunc f() {\n\tif let x: int8 = 300; x > 0 {\n\t}\n}",
			want: "x.gol:3:19: cannot use 300 (untyped int constant) as int8 value in variable declaration (overflows)",
		},
		{
			name: "assignment to a typed header const",
			src:  "package p\nfunc f() {\n\tfor (const i: int64 = 0; i < 3; i++) {\n\t}\n}",
			want: "x.gol:3:34: cannot assign to i: declared with const",
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
			want: "x.gol:4:13: p.Z undefined",
		},
		{
			name: "destructuring default on a field that is not comparable",
			src:  "package p\ntype S struct{ Xs []int }\ntype C struct{ S S }\nfunc f(c C) S {\n\tconst {S = S{}} = c\n\treturn S\n}",
			want: "x.gol:5:8: a destructuring default needs a type that is comparable or can be nil, not S",
		},
		{
			name: "destructuring default with the wrong type",
			src:  "package p\ntype C struct{ Name string }\nfunc f(c C) string {\n\tconst {Name = 5} = c\n\treturn Name\n}",
			want: "x.gol:4:8: cannot use 5 (untyped int constant) as string value in destructuring default",
		},
		{
			name: "destructuring default that can fail in a try",
			src:  "package p\nimport \"strconv\"\ntype C struct{ Port int }\nfunc f(c C, s string) int {\n\ttry {\n\t\tconst {Port = strconv.Atoi(s)} = c\n\t\treturn Port\n\t} catch {\n\t\treturn 0\n\t}\n}",
			want: "only runs conditionally here",
		},
		{
			name: "parameter destructuring unknown field",
			src:  "package p\ntype P struct{ X int }\nfunc f({Z}: P) int {\n\treturn Z\n}",
			want: "x.gol:3:21: p2.Z undefined",
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
			name: "address of an optional chain",
			src:  "package p\ntype P struct{ X int }\nfunc f(v *P) *int {\n\treturn &v?.X\n}",
			want: "x.gol:4:10: cannot take the address of an optional chain",
		},
		{
			name: "address of a parenthesized optional chain",
			src:  "package p\ntype P struct{ X int }\nfunc f(v *P) *int {\n\treturn &(v?.X)\n}",
			want: "x.gol:4:11: cannot take the address of an optional chain",
		},
		{
			name: "assignment to an optional chain",
			src:  "package p\ntype P struct{ X int; Xs []int }\nfunc f(v *P) {\n\tv?.X = 1\n}",
			want: "x.gol:4:2: an optional chain cannot be the target of an assignment",
		},
		{
			name: "compound assignment to an optional chain",
			src:  "package p\ntype P struct{ X int; Xs []int }\nfunc f(v *P) {\n\tv?.X += 1\n}",
			want: "x.gol:4:2: an optional chain cannot be the target of an assignment",
		},
		{
			name: "increment of an optional chain",
			src:  "package p\ntype P struct{ X int; Xs []int }\nfunc f(v *P) {\n\tv?.X++\n}",
			want: "x.gol:4:2: an optional chain cannot be the target of an assignment",
		},
		{
			name: "assignment to a parenthesized optional chain",
			src:  "package p\ntype P struct{ X int; Xs []int }\nfunc f(v *P) {\n\t(v?.X) = 1\n}",
			want: "x.gol:4:3: an optional chain cannot be the target of an assignment",
		},
		{
			name: "assignment to an index of an optional chain",
			src:  "package p\ntype P struct{ X int; Xs []int }\nfunc f(v *P) {\n\tv?.Xs[0] = 1\n}",
			want: "x.gol:4:2: an optional chain cannot be the target of an assignment",
		},
		{
			name: "assignment through a dereferenced optional chain",
			src:  "package p\ntype T struct{ P *int }\nfunc f(v *T) {\n\t*v?.P = 1\n}",
			want: "x.gol:4:3: cannot dereference an optional chain; it gives nil when the chain is skipped",
		},
		{
			name: "increment through a dereferenced optional chain",
			src:  "package p\ntype T struct{ P *int }\nfunc f(v *T) {\n\t(*v?.P)++\n}",
			want: "x.gol:4:4: cannot dereference an optional chain; it gives nil when the chain is skipped",
		},
		{
			name: "assignment through a dereferenced parenthesized optional chain",
			src:  "package p\ntype T struct{ P *int }\nfunc f(v *T) {\n\t*(v?.P) = 1\n}",
			want: "x.gol:4:4: cannot dereference an optional chain; it gives nil when the chain is skipped",
		},
		{
			name: "read of a dereferenced optional chain",
			src:  "package p\ntype T struct{ P *int }\nfunc f(v *T) {\n\tprintln(*v?.P)\n}",
			want: "x.gol:4:11: cannot dereference an optional chain; it gives nil when the chain is skipped",
		},
		{
			name: "nullish assignment to a value that is never nil",
			src:  "package p\nfunc f(n int) int {\n\tn ??= 1\n\treturn n\n}",
			want: "x.gol:3:2: left side of ??= must be a pointer, map, slice, interface, func, or chan, not int",
		},
		{
			name: "nullish assignment does not take the address",
			src:  "package p\nfunc f(p *int) {\n\tp ??= 1\n}",
			want: "x.gol:3:8: cannot use 1 (untyped int constant) as *int value in assignment",
		},
		{
			name: "nullish assignment in a header",
			src:  "package p\nfunc f(p, q *int) {\n\tif p ??= q; p != nil {\n\t}\n}",
			want: "x.gol:3:5: ??= can only be used as a statement in a block",
		},
		{
			name: "nullish assignment with two values",
			src:  "package p\nfunc f(p, q *int) {\n\tp, q ??= q, p\n}",
			want: "x.gol:3:2: ??= needs one value on each side",
		},
		{
			name: "nullish assignment to a const variable",
			src:  "package p\nfunc g() *int { return nil }\nfunc f(q *int) {\n\tconst p = g()\n\tp ??= q\n}",
			want: "x.gol:5:2: cannot assign to p: declared with const",
		},
		{
			name: "nullish assignment of a call that can fail",
			src:  "package p\nimport \"os\"\nfunc f(file *os.File) {\n\ttry {\n\t\tfile ??= os.Open(\"x\")\n\t} catch {\n\t}\n}",
			want: "x.gol:5:12: assignment mismatch: 1 variable but os.Open returns 2 values",
		},
		{
			name: "nullish assignment of a nested call that can fail",
			src:  "package p\nimport \"strconv\"\nfunc f(p *int) {\n\ttry {\n\t\tp ??= new(strconv.Atoi(\"1\"))\n\t} catch {\n\t}\n}",
			want: "x.gol:5:13: this call can fail but only runs conditionally here",
		},
		{
			name: "optional index on a string",
			src:  "package p\nfunc f(s string) byte {\n\treturn s?.[0]\n}",
			want: "x.gol:3:10: ?.[ needs a map, a slice, or a pointer to an array on its left, not string",
		},
		{
			name: "optional index on an array value",
			src:  "package p\nfunc f(a [2]int) int {\n\treturn a?.[0] ?? 1\n}",
			want: "x.gol:3:10: ?.[ needs a map, a slice, or a pointer to an array on its left, not [2]int",
		},
		{
			name: "optional slice expression",
			src:  "package p\nfunc f(s []int) []int {\n\treturn s?.[1:]\n}",
			want: "x.gol:3:10: ?.[ cannot be used with a slice expression",
		},
		{
			name: "nullish coalescing mixed with ||",
			src:  "package p\nfunc f(a *bool, b bool) bool {\n\treturn a ?? b || true\n}",
			want: "x.gol:3:9: ?? cannot be mixed with || or && without parentheses",
		},
		{
			name: "optional call returning error assigned in try",
			src:  "package p\nimport \"io\"\nfunc f(w io.Writer, b []byte) {\n\ttry {\n\t\tlet n = w?.Write(b)\n\t\tprintln(n)\n\t} catch {\n\t}\n}",
			want: "x.gol:5:7: the error from an optional call is checked only if the call is a statement; use an explicit nil check",
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
			name: "arrow that only returns nil",
			src:  "package p\nfunc f() {\n\tlet g = (x int) => nil\n\t_ = g\n}",
			want: "x.gol:3:10: " + errNilArrowResult,
		},
		{
			name: "arrow block that only returns nil",
			src:  "package p\nfunc f(c bool) {\n\tlet g = () => {\n\t\tif c {\n\t\t\treturn nil\n\t\t}\n\t\treturn nil\n\t}\n\t_ = g\n}",
			want: "x.gol:3:10: " + errNilArrowResult,
		},
		{
			name: "arrow with one result that is always nil",
			src:  "package p\nfunc f() {\n\tlet g = (c bool) => {\n\t\tif c {\n\t\t\treturn nil, nil\n\t\t}\n\t\treturn nil, 1\n\t}\n\t_ = g\n}",
			want: "x.gol:3:10: " + errNilArrowResult,
		},
		{
			name: "arrow returning an arrow without parameter types",
			src:  "package p\nfunc f() {\n\tlet g = () => (x) => x\n\t_ = g\n}",
			want: "x.gol:3:16: cannot infer the parameter types of this arrow function",
		},
		{
			name: "arrow returning an arrow that only returns nil",
			src:  "package p\nfunc f() {\n\tlet g = () => () => nil\n\t_ = g\n}",
			want: "x.gol:3:16: " + errNilArrowResult,
		},
		{
			name: "map callback that only returns nil",
			src:  "package p\nfunc f(xs []int) {\n\tlet ys = xs.map(x => nil)\n\t_ = ys\n}",
			want: "x.gol:3:18: " + errNilArrowResult,
		},
		{
			name: "filter result type hidden by a local type",
			src:  "package p\ntype inner struct{}\ntype item struct{ In inner }\nvar global = []item{{}}\nfunc keep(x item) bool { return true }\nfunc pick() *item { return nil }\nfunc f() {\n\ttype item int\n\t_ = global.filter(keep)\n}",
			want: "x.gol:9:6: the inferred type []item cannot be written here, because a local declaration hides the name; rename the local declaration",
		},
		{
			name: "includes element type hidden by a local type",
			src:  "package p\ntype inner struct{}\ntype item struct{ In inner }\nvar global = []item{{}}\nfunc keep(x item) bool { return true }\nfunc pick() *item { return nil }\nfunc f() {\n\ttype item int\n\t_ = global.includes(global[0])\n}",
			want: "x.gol:9:6: the inferred type item cannot be written here, because a local declaration hides the name; rename the local declaration",
		},
		{
			name: "slice spread element type hidden by a local type",
			src:  "package p\ntype inner struct{}\ntype item struct{ In inner }\nvar global = []item{{}}\nfunc keep(x item) bool { return true }\nfunc pick() *item { return nil }\nfunc f() {\n\ttype item int\n\t_ = [...global, global[0]]\n}",
			want: "x.gol:9:6: the inferred type item cannot be written here, because a local declaration hides the name; rename the local declaration",
		},
		{
			name: "nullish result type hidden by a local type",
			src:  "package p\ntype inner struct{}\ntype item struct{ In inner }\nvar global = []item{{}}\nfunc keep(x item) bool { return true }\nfunc pick() *item { return nil }\nfunc f() {\n\ttype item int\n\t_ = pick() ?? global[0]\n}",
			want: "x.gol:9:6: the inferred type item cannot be written here, because a local declaration hides the name; rename the local declaration",
		},
		{
			name: "optional chain result type hidden by a local type",
			src:  "package p\ntype inner struct{}\ntype item struct{ In inner }\nvar global = []item{{}}\nfunc keep(x item) bool { return true }\nfunc pick() *item { return nil }\nfunc f() {\n\ttype inner int\n\t_ = pick()?.In\n}",
			want: "x.gol:9:6: the inferred type inner cannot be written here, because a local declaration hides the name; rename the local declaration",
		},
		{
			name: "finally return type hidden by a local type",
			src:  "package p\ntype inner struct{}\ntype item struct{ In inner }\nvar global = []item{{}}\nfunc keep(x item) bool { return true }\nfunc pick() *item { return nil }\nfunc f() item {\n\ttype item int\n\ttry {\n\t\treturn global[0]\n\t} finally {\n\t\tprintln()\n\t}\n}",
			want: "x.gol:9:2: the inferred type item cannot be written here, because a local declaration hides the name; rename the local declaration",
		},
		{
			name: "arrow whose first use gives no types",
			src:  "package p\nimport \"fmt\"\nfunc f() {\n\tconst g = x => x\n\tfmt.Println(g)\n\t_ = g(1)\n}",
			want: "x.gol:4:12: cannot infer the parameter types of this arrow function",
		},
		{
			name: "arrow at package level without context",
			src:  "package p\nlet g = x => x\nfunc f() int {\n\treturn g(1)\n}",
			want: "x.gol:2:9: cannot infer the parameter types of this arrow function",
		},
		{
			name: "later call with another type than the first call",
			src:  "package p\nfunc f() {\n\tconst id = x => x\n\t_ = id(1)\n\t_ = id(\"a\")\n}",
			want: "x.gol:5:9: cannot use \"a\" (untyped string constant) as int value in argument to id",
		},
		{
			name: "first call with untyped nil",
			src:  "package p\nfunc f() {\n\tconst g = x => x\n\t_ = g(nil)\n}",
			want: "x.gol:3:12: cannot infer the parameter types of this arrow function",
		},
		{
			name: "arrow body that does not match its result type",
			src:  "package p\nfunc f() {\n\tlet g = (x: int): string => x\n\t_ = g\n}",
			want: "x.gol:3:30: cannot use x (variable of type int) as string value in return statement",
		},
		{
			name: "go parameter order with arrow result",
			src:  "package p\nfunc f() {\n\tlet g = (x int): string => \"\"\n\t_ = g\n}",
			want: "x.gol:3:11: write name: type for every parameter",
		},
		{
			name: "arrow with result type and context with other parameters",
			src:  "package p\nimport \"sort\"\nfunc f(xs []int) {\n\tsort.Slice(xs, (i): bool => true)\n}",
			want: "x.gol:4:17: cannot infer the parameter types of this arrow function",
		},
		{
			name: "forEach used as a value",
			src:  "package p\nfunc f(xs []int) {\n\tlet y = xs.forEach(x => println(x))\n\t_ = y\n}",
			want: "x.gol:3:10: forEach can only be used as a statement",
		},
		{
			name: "map delete used as a value",
			src:  "package p\nfunc f(m map[string]int) {\n\tlet y = m.delete(\"a\")\n\t_ = y\n}",
			want: "x.gol:3:10: delete can only be used as a statement",
		},
		{
			name: "map has without a key",
			src:  "package p\nfunc f(m map[string]int) bool {\n\treturn m.has()\n}",
			want: "x.gol:3:15: not enough arguments in call to has",
		},
		{
			name: "map key of the wrong type",
			src:  "package p\nfunc f(m map[string]int) bool {\n\treturn m.has(1)\n}",
			want: "x.gol:3:15: cannot use 1 (untyped int constant) as string value in argument to has",
		},
		{
			name: "sort without a comparator on unordered elements",
			src:  "package p\ntype P struct{ X int }\nfunc f(ps []P) {\n\tps.sort()\n}",
			want: "x.gol:4:2: P does not satisfy",
		},
		{
			name: "slice with too many arguments",
			src:  "package p\nfunc f(xs []int) []int {\n\treturn xs.slice(1, 2, 3)\n}",
			want: "x.gol:3:9: slice expects 1 to 2 argument(s)",
		},
		{
			name: "slice with a spread argument",
			src:  "package p\nfunc f(xs []int, bounds []int) []int {\n\treturn xs.slice(1, bounds...)\n}",
			want: "x.gol:3:21: cannot use bounds (variable of type []int) as int value in argument to slice",
		},
		{
			name: "flatMap callback that does not return a slice",
			src:  "package p\nfunc f(xs []int) []int {\n\treturn xs.flatMap(x => x)\n}",
			want: "x.gol:3:20: flatMap callback has type func(x int) int, want func(int) []R",
		},
		{
			name: "assignment to length",
			src:  "package p\nfunc f(xs []int) {\n\txs.length = 0\n}",
			want: "x.gol:3:2: cannot assign to len(xs)",
		},
		{
			name: "length of a pointer to a slice",
			src:  "package p\nfunc f(xs *[]int) int {\n\treturn xs.length\n}",
			want: "x.gol:3:12: xs.length undefined",
		},
		{
			name: "reduce without an initial value keeps the element type",
			src:  "package p\nfunc f(xs []int) int {\n\treturn xs.reduce((acc, n) => acc + float64(n))\n}",
			want: "x.gol:3:31: invalid operation: acc + float64(n) (mismatched types int and float64)",
		},
		{
			name: "string slice with too many arguments",
			src:  "package p\nfunc f(s string) string {\n\treturn s.slice(1, 2, 3)\n}",
			want: "x.gol:3:9: slice expects 1 or 2 arguments",
		},
		{
			name: "string method with a wrong argument type",
			src:  "package p\nfunc f(s string) bool {\n\treturn s.includes(1)\n}",
			want: "x.gol:3:20: cannot use 1 (untyped int constant) as string value in argument to includes",
		},
		{
			name: "string slice with a shadowed builtin",
			src:  "package p\nfunc f(s string, max int) string {\n\treturn s.slice(max)\n}",
			want: "x.gol:3:9: slice needs the builtin max, but a declaration shadows it here; rename that declaration",
		},
		{
			name: "string method needs strings but the name is taken",
			src:  "package p\nfunc f(strings string) bool {\n\treturn strings.includes(\"a\")\n}",
			want: "x.gol:3:9: includes needs package strings, but the name strings is already used in this file",
		},
		{
			name: "switch missing union variants",
			src:  "package p\ntype A struct{}\ntype B struct{}\ntype C struct{}\ntype U = A | B | C\nfunc f(u U) {\n\tswitch u.(type) {\n\tcase A:\n\t}\n}",
			want: "x.gol:7:2: switch on U is missing cases: B, C",
		},
		{
			name: "switch missing generic union variants",
			src:  "package p\ntype A[T any] struct{}\ntype B[T any] struct{}\ntype U[T any] = A[T] | *B[T]\nfunc f(u U[int]) {\n\tswitch u.(type) {\n\tcase A[int]:\n\t}\n}",
			want: "x.gol:6:2: switch on U[int] is missing cases: *B[int]",
		},
		{
			name: "switch missing generic union variants in a generic function",
			src:  "package p\ntype A[T any] struct{}\ntype B[T any] struct{}\ntype U[T any] = A[T] | B[T]\nfunc f[T any](u U[T]) {\n\tswitch u.(type) {\n\tcase A[T]:\n\t}\n}",
			want: "x.gol:6:2: switch on U[T] is missing cases: B[T]",
		},
		{
			name: "generic union variant with another type argument",
			src:  "package p\ntype A[T any] struct{}\ntype B[T any] struct{}\ntype U[T any] = A[T] | B[T]\nfunc f() U[int] {\n\treturn A[string]{}\n}",
			want: "x.gol:6:9: cannot use A[string]{} (value of struct type A[string]) as U[int] value in return statement: A[string] does not implement U[int] (wrong type for method isU)",
		},
		{
			name: "switch missing variants of a union in a type group",
			src:  "package p\ntype (\n\tA struct{}\n\tB struct{}\n\tU = A | B\n)\nfunc f(u U) {\n\tswitch u.(type) {\n\tcase A:\n\t}\n}",
			want: "x.gol:8:2: switch on U is missing cases: B",
		},
		{
			name: "union variant from another package in a type group",
			src:  "package p\nimport \"time\"\ntype A struct{}\ntype (\n\tID int\n\tU = A | time.Time\n)\n",
			want: "x.gol:6:2: union variant time.Time must be a type declared in this package",
		},
		{
			name: "switch missing literal union values",
			src:  "package p\ntype M = \"a\" | \"b\" | \"c\"\nfunc f(m M) {\n\tswitch m {\n\tcase MA:\n\t}\n}",
			want: "x.gol:4:2: switch on M is missing cases: MB, MC",
		},
		{
			name: "constant that is not in a literal union",
			src:  "package p\ntype M = \"a\" | \"b\"\nfunc f() M {\n\treturn \"c\"\n}",
			want: "x.gol:4:9: \"c\" is not a value of M",
		},
		{
			name: "comparison with a constant that is not in a literal union",
			src:  "package p\ntype Code = 1 | 2\nfunc f(c Code) bool {\n\treturn c == 1 || c == Code(3)\n}",
			want: "x.gol:4:24: 3 is not a value of Code",
		},
		{
			name: "switch missing enum members",
			src:  "package p\nenum Color { Red, Green, Blue }\nfunc f(c Color) {\n\tswitch c {\n\tcase Color.Green:\n\t}\n}",
			want: "x.gol:4:2: switch on Color is missing cases: ColorRed, ColorBlue",
		},
		{
			name: "unknown enum member",
			src:  "package p\nenum Color { Red }\nfunc f() Color {\n\treturn Color.Purple\n}",
			want: "x.gol:4:15: Color.Purple undefined",
		},
		{
			name: "enum with its own String method",
			src:  "package p\nenum Color { Red }\nfunc (c Color) String() string { return \"\" }\n",
			want: "x.gol:3:16: method Color.String already declared",
		},
		{
			name: "assignment to a variable that instanceof narrows",
			src:  "package p\ntype A struct{}\nfunc f(v any) {\n\tif (v instanceof A) {\n\t\tv = A{}\n\t}\n}",
			want: "x.gol:5:3: cannot assign to v: narrowed by instanceof",
		},
		{
			name: "instanceof on an error needs package errors but the name is taken",
			src:  "package p\nimport \"io/fs\"\nfunc f(errors int, err error) string {\n\tif (err instanceof *fs.PathError) {\n\t\treturn err.Path\n\t}\n\treturn \"\"\n}",
			want: "x.gol:4:6: instanceof needs package errors, but the name errors is already used in this file",
		},
		{
			name: "instanceof on a value that is not an interface",
			src:  "package p\ntype A struct{}\nfunc f(v A) {\n\tif (v instanceof A) {\n\t}\n}",
			want: "x.gol:4:6: invalid operation: v (variable of struct type A) is not an interface",
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
			name: "defer in a try block with catch panic",
			src:  "package p\nfunc f(g func()) {\n\ttry {\n\t\tdefer g()\n\t} catch panic {\n\t}\n}",
			want: "x.gol:4:3: defer cannot be used directly in a try block with catch panic; it would run when the try block ends",
		},
		{
			name: "catch panic with an unused panic value",
			src:  "package p\nfunc f(g func()) int {\n\ttry {\n\t\tg()\n\t\treturn 1\n\t} catch panic (p) {\n\t\tprintln(\"x\")\n\t}\n}",
			want: "x.gol:9:1: missing return",
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
			name: "assignment to a const type switch binding after an init statement",
			src:  "package p\nfunc f(v any) {\n\tswitch let w = v; const s = w.(type) {\n\tcase int:\n\t\ts = 2\n\t\tprintln(s)\n\t}\n}",
			want: "x.gol:5:3: cannot assign to s: declared with const",
		},
		{
			name: "missing union case in a type switch with an init statement",
			src:  "package p\ntype A struct{}\ntype B struct{}\ntype U = A | B\nfunc f(u U) {\n\tswitch let w = u; const s = w.(type) {\n\tcase A:\n\t\tprintln(s)\n\t}\n}",
			want: "switch on U is missing cases: B",
		},
		{
			name: "positional field in struct spread",
			src:  "package p\ntype P struct{ X, Y int }\nfunc f(p P) P {\n\treturn P{...p, 1}\n}",
			want: "x.gol:4:17: fields after a struct spread must be named",
		},
		{
			name: "local variable hides the package of an arrow result type",
			src:  "package p\nimport \"os\"\nfunc h() *os.File { return nil }\nfunc f() {\n\tlet os = 1\n\t_ = os\n\tconst g = () => h()\n\t_ = g\n}",
			want: "x.gol:7:12: the inferred type *os.File cannot be written here, because a local declaration hides the name; rename the local declaration",
		},
		{
			name: "local variable hides the package of an arrow parameter type",
			src:  "package p\nimport \"os\"\nfunc each(f func(*os.File)) {}\nfunc f() {\n\tlet os = 1\n\t_ = os\n\teach(file => println(file))\n}",
			want: "x.gol:7:7: the inferred type *os.File cannot be written here, because a local declaration hides the name; rename the local declaration",
		},
		{
			name: "local variable hides the package of an array method result",
			src:  "package p\nimport \"os\"\nfunc f(files []*os.File) {\n\tlet os = 1\n\t_ = os\n\t_ = files.filter(file => file != nil)\n}",
			want: "x.gol:6:19: the inferred type *os.File cannot be written here, because a local declaration hides the name; rename the local declaration",
		},
		{
			name: "local variable hides the fmt import of a template literal",
			src:  "package p\nimport \"fmt\"\nvar _ = fmt.Sprint\nfunc f(n int) string {\n\tlet fmt = 2\n\t_ = fmt\n\treturn t`${n}`\n}",
			want: "x.gol:7:9: template literal needs package fmt, but the name fmt is already used in this file",
		},
		{
			name: "local variable hides the slices import of a spread",
			src:  "package p\nimport \"slices\"\nvar _ = slices.Max[[]int]\nfunc f(p []int64) []int64 {\n\tlet slices = 2\n\t_ = slices\n\treturn [...p, int64(1)]\n}",
			want: "x.gol:7:9: spread needs package slices and the element type, but a needed name is already used in this file",
		},
		{
			name: "array method marker in argument message",
			src:  "package p\nfunc f(xs []int) bool {\n\treturn xs.includes(\"a\")\n}",
			want: "x.gol:3:21: cannot use \"a\" (untyped string constant) as int value in argument to includes",
		},
		{
			name: "array method marker in expression",
			src:  "package p\nfunc f(xs []int) int {\n\treturn xs.filter(n => n > 1).indexOf(\"a\")\n}",
			want: "x.gol:3:39: cannot use \"a\" (untyped string constant) as int value in argument to indexOf",
		},
		{
			name: "map callback without a result",
			src:  "package p\nfunc f(xs []int) []int {\n\treturn xs.map(x => { println(x) })\n}",
			want: "x.gol:3:16: map callback must return a value",
		},
		{
			name: "slice spread element of another type",
			src:  "package p\nfunc f(xs []int64, n int) []int64 {\n\treturn [...xs, n]\n}",
			want: "x.gol:3:17: slice spread element n has type int, want int64",
		},
		{
			name: "slice spread of another slice type",
			src:  "package p\nfunc f(xs []int64, ys []int) []int64 {\n\treturn [...xs, ...ys]\n}",
			want: "x.gol:3:20: slice spread part ...ys has type []int, want []int64",
		},
		{
			name: "slice spread element marker in argument message",
			src:  "package p\nfunc f(xs []int64) []int64 {\n\treturn [...xs, int64(1), \"a\"]\n}",
			want: "x.gol:3:27: cannot use \"a\" (untyped string constant) as int64 value in argument to slice spread",
		},
		{
			name: "slice spread in expression",
			src:  "package p\nfunc f(xs []int64) []string {\n\tlet ys: []string = [...xs, int64(1)]\n\treturn ys\n}",
			want: "x.gol:3:21: cannot use [...xs, int64(1)] (value of type []int64) as []string value in variable declaration",
		},
		{
			name: "struct spread of another type",
			src:  "package p\ntype P struct{ X int }\ntype Q struct{ X int }\nfunc f(q Q) P {\n\treturn P{...q, X: 1}\n}",
			want: "x.gol:5:17: struct spread of type Q into P",
		},
		{
			name: "template literal in expression",
			src:  "package p\nfunc f(n int) string {\n\tconst y = t`a ${n}` * 2\n\treturn y\n}",
			want: "x.gol:3:12: invalid operation: t`a ${n}` * 2 (mismatched types string and untyped int)",
		},
		{
			name: "nullish coalescing marker in argument message",
			src:  "package p\nfunc f(s *int) int {\n\treturn s ?? \"x\"\n}",
			want: "x.gol:3:14: cannot use \"x\" (untyped string constant) as int value in argument to ??",
		},
		{
			name: "package function shadows a builtin that an array method needs",
			src:  "package p\nfunc len(s string) int { return 0 }\nfunc f(xs []int) []int {\n\treturn xs.map(x => x * 2)\n}",
			want: "x.gol:4:9: map needs the builtin len, but a declaration shadows it here; rename that declaration",
		},
		{
			name: "local variable shadows a builtin that an array method needs",
			src:  "package p\nfunc f(xs []int) []int {\n\tlet append = 3\n\t_ = append\n\treturn xs.filter(x => x > 1)\n}",
			want: "x.gol:5:9: filter needs the builtin append, but a declaration shadows it here; rename that declaration",
		},
		{
			name: "local variable shadows the builtin that length needs",
			src:  "package p\nfunc f(xs []int) int {\n\tconst len = 3\n\treturn xs.length + len\n}",
			want: "x.gol:4:9: length needs the builtin len, but a declaration shadows it here; rename that declaration",
		},
		{
			name: "package function shadows the builtin that length needs",
			src:  "package p\nfunc len(s string) int { return 0 }\nfunc f(s string) int {\n\treturn s.length\n}",
			want: "x.gol:4:9: length needs the builtin len, but a declaration shadows it here; rename that declaration",
		},
		{
			name: "parameter shadows a builtin that try needs",
			src:  "package p\nimport \"os\"\nfunc f(nil int) {\n\ttry {\n\t\tos.Remove(\"x\")\n\t} catch {\n\t}\n}",
			want: "x.gol:5:3: try needs the builtin nil, but a declaration shadows it here; rename that declaration",
		},
		{
			name: "parameter shadows a builtin that optional chaining needs",
			src:  "package p\ntype P struct{ X *int }\nfunc f(p *P, nil int) *int {\n\treturn p?.X\n}",
			want: "x.gol:4:9: optional chain needs the builtin nil, but a declaration shadows it here; rename that declaration",
		},
		{
			name: "parameter shadows a builtin that a template literal needs",
			src:  "package p\nfunc f(x float32, float64 int) string {\n\treturn t`${x}`\n}",
			want: "x.gol:3:13: template literal needs the builtin float64, but a declaration shadows it here; rename that declaration",
		},
		{
			name: "throw outside try in a function without an error result",
			src:  "package p\nimport \"errors\"\nfunc f() int {\n\tthrow errors.New(\"x\")\n}",
			want: "x.gol:4:2: throw needs an enclosing try or a function whose last result is error",
		},
		{
			name: "throw of a value that is not an error",
			src:  "package p\nfunc f() error {\n\tthrow 1\n}",
			want: "x.gol:3:8: cannot use 1 (constant of type int) as error value in throw",
		},
		{
			name: "unmatched errors with no outer try in a function without an error result",
			src:  "package p\nimport \"os\"\nfunc f() {\n\ttry {\n\t\tos.Remove(\"x\")\n\t} catch (e is os.ErrNotExist) {\n\t}\n}",
			want: "x.gol:4:2: errors that no catch matches need an outer try or a function whose last result is error; add a last catch (e)",
		},
		{
			name: "catch type that is not an error",
			src:  "package p\nimport \"os\"\nfunc f() {\n\ttry {\n\t\tos.Remove(\"x\")\n\t} catch (e: int) {\n\t} catch {\n\t}\n}",
			want: "x.gol:6:14: catch type int does not implement error",
		},
		{
			name: "catch value that is not an error",
			src:  "package p\nimport \"os\"\nfunc f() {\n\ttry {\n\t\tos.Remove(\"x\")\n\t} catch (e is 1) {\n\t} catch {\n\t}\n}",
			want: "x.gol:6:16: cannot use 1 (constant of type int) as error value in catch",
		},
		{
			name: "typed catch needs errors but the name is taken",
			src:  "package p\nimport \"os\"\nfunc f(errors int) {\n\ttry {\n\t\tos.Remove(\"x\")\n\t} catch (e is os.ErrNotExist) {\n\t} catch {\n\t}\n}",
			want: "x.gol:4:2: typed catch needs package errors, but the name errors is already used in this file",
		},
		{
			name: "try expression on a call without an error",
			src:  "package p\nfunc g() int { return 1 }\nfunc f() int {\n\treturn try g() catch 0\n}",
			want: "x.gol:4:13: a try expression needs a call that returns a value and an error",
		},
		{
			name: "try expression with a fallback of another type",
			src:  "package p\nimport \"strconv\"\nfunc f() int {\n\treturn try strconv.Atoi(\"1\") catch \"x\"\n}",
			want: "x.gol:4:37: cannot use \"x\" (untyped string constant) as int value in try expression fallback",
		},
		{
			name: "fallible call in a try expression fallback",
			src:  "package p\nimport \"strconv\"\nfunc f() int {\n\ttry {\n\t\treturn try strconv.Atoi(\"x\") catch strconv.Atoi(\"1\")\n\t} catch {\n\t\treturn 0\n\t}\n}",
			want: "x.gol:5:38: this call can fail but only runs conditionally here",
		},
		{
			name: "tag function with the wrong signature",
			src:  "package p\nfunc up(s string) string { return s }\nfunc f() string {\n\treturn up`a`\n}",
			want: "x.gol:4:11: cannot use []string{…} (value of type []string) as string value in argument to up",
		},
		{
			name: "struct spread after a field",
			src:  "package p\ntype P struct{ X, Y int }\nfunc f(p P) P {\n\treturn P{X: 1, ...p}\n}",
			want: "x.gol:4:20: struct spread must be the first element",
		},
		{
			name: "second struct spread",
			src:  "package p\ntype P struct{ X, Y int }\nfunc f(p, q P) P {\n\treturn {...p, ...q}\n}",
			want: "x.gol:4:19: struct spread must be the first element",
		},
		{
			name: "spread in braces of a slice type",
			src:  "package p\nfunc f(a []int) []int {\n\treturn []int{...a}\n}",
			want: "x.gol:3:9: spread in braces needs a map or struct type; use [...a] for slices",
		},
		{
			name: "call spread to a function that is not variadic",
			src:  "package p\nfunc g(a, b int) int { return a + b }\nfunc f(xs []int) int {\n\treturn g(...xs)\n}",
			want: "x.gol:4:16: cannot use ... in call to non-variadic g",
		},
		{
			name: "optional field value of the wrong type",
			src:  "package p\ntype C struct{ Host? string }\nvar c = C{Host: 5}",
			want: "x.gol:3:17: cannot use 5 (untyped int constant) as *string value in struct literal",
		},
		{
			name: "optional field value that overflows",
			src:  "package p\ntype C struct{ Port? int8 }\nvar c = C{Port: 300}",
			want: "x.gol:3:17: cannot use 300 (untyped int constant) as int8 value in struct literal (overflows)",
		},
		{
			name: "invalid regex literal",
			src:  "package p\nfunc f(s string) bool {\n\treturn /a(?=b)/.MatchString(s)\n}",
			want: "x.gol:3:9: error parsing regexp: invalid or unsupported Perl syntax: `(?=`",
		},
		{
			name: "regex literal text is not Go syntax",
			src:  "package p\nfunc f(s string) bool {\n\tlet ok = /a:=\\{/.MatchString(s)\n\tvar x int\n\treturn ok\n}",
			want: "x.gol:4:2: use let instead of var in a function",
		},
		{
			name: "regex literal needs package regexp but the name is taken",
			src:  "package p\nfunc f(regexp string) bool {\n\treturn /a/.MatchString(regexp)\n}",
			want: "regex literal needs package regexp, but the name regexp is already used in this file",
		},
		{
			name: "impl block that does not implement its interface",
			src:  "package p\nimport \"io\"\ntype R struct{}\nimpl R implements io.Reader {\n\tfunc Close() error { return nil }\n}",
			want: "x.gol:4:1: cannot use (*R)(nil) (value of type *R) as io.Reader value in variable declaration: *R does not implement io.Reader (missing method Read)",
		},
		{
			name: "type error in an impl method",
			src:  "package p\ntype R struct{}\nimpl R {\n\tfunc Size() int { return this.n }\n}",
			want: "x.gol:4:32: this.n undefined",
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
			name: "yield in a function literal inside a generator",
			src:  "package p\nimport \"iter\"\nfunc* g() iter.Seq[int] {\n\tfunc() {\n\t\tyield 1\n\t}()\n}",
			want: "x.gol:5:3: yield must be directly in a generator body, not in a nested function",
		},
		{
			name: "using with a fallible call outside try",
			src:  "package p\nimport \"os\"\nfunc f(p string) {\n\tusing file = os.Open(p)\n\t_ = file\n}",
			want: "x.gol:4:8: using with a call that can fail must be in a try block",
		},
		{
			name: "using in a loop",
			src:  "package p\nimport \"os\"\nfunc f(paths []string) {\n\tfor (const p of paths) {\n\t\ttry {\n\t\t\tusing file = os.Open(p)\n\t\t\t_ = file\n\t\t} catch {\n\t\t}\n\t}\n}",
			want: "x.gol:6:10: using is not allowed in a loop, because Close runs only when the function returns; move the loop body into a function",
		},
		{
			name: "assignment to a using variable",
			src:  "package p\ntype R struct{}\nfunc (R) Close() {}\nfunc f() {\n\tusing r = R{}\n\tr = R{}\n}",
			want: "x.gol:6:2: cannot assign to r: declared with const",
		},
		{
			name: "using a value without Close",
			src:  "package p\nfunc f() {\n\tusing n = 1\n\t_ = n\n}",
			want: "n.Close undefined (type int has no field or method Close)",
		},
		{
			name: "as on a value that is not an interface",
			src:  "package p\nfunc f(n int) int {\n\treturn n as int\n}",
			want: "x.gol:3:9: invalid operation: n (variable of type int) is not an interface",
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

// TestErrorsReportedOnce checks that one problem gives one diagnostic.
func TestErrorsReportedOnce(test *testing.T) {
	testCases := []struct {
		name string
		src  string
	}{
		{
			name: "length with a shadowed len",
			src:  "package p\nfunc f(xs []int) int {\n\tconst len = 3\n\treturn xs.length + len\n}",
		},
		{
			name: "optional chain on a struct value with a call moved out of try",
			src:  "package p\nimport \"strconv\"\ntype T struct{}\nfunc (T) Get(n int) int { return n }\nfunc f(v T) int {\n\ttry {\n\t\treturn v?.Get(strconv.Atoi(\"1\"))\n\t} catch {\n\t\treturn 0\n\t}\n}",
		},
		{
			name: "optional chain on a struct value inside try",
			src:  "package p\nimport \"io\"\ntype T struct{ X int }\nfunc f(v T, c io.Closer) int {\n\ttry {\n\t\tc.Close()\n\t\treturn v?.X\n\t} catch {\n\t\treturn 0\n\t}\n}",
		},
		{
			name: "coalesce on an optional chain on a struct value with a call moved out of try",
			src:  "package p\nimport \"strconv\"\ntype T struct{}\nfunc (T) Get(n int) *int { return &n }\nfunc f(v T) int {\n\ttry {\n\t\treturn v?.Get(strconv.Atoi(\"1\")) ?? 0\n\t} catch {\n\t\treturn 0\n\t}\n}",
		},
	}
	for _, testCase := range testCases {
		test.Run(testCase.name, func(test *testing.T) {
			_, err := Transpile(Config{Importer: importer.Default()}, []Input{{Path: "x.gol", Src: []byte(testCase.src)}})
			var errorList scanner.ErrorList
			if !errors.As(err, &errorList) {
				test.Fatalf("want a scanner.ErrorList, got %v", err)
			}
			if len(errorList) != 1 {
				test.Errorf("got %d errors, want 1:\n%v", len(errorList), err)
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
	expectPanicAt(test, src, "prog.gol:17")
}

// TestLineDirectivesAfterMultilineUnion checks that a union written over
// several lines does not shift the lines after it.
func TestLineDirectivesAfterMultilineUnion(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	src := `package main

type A struct{}
type B struct{}

type U = A |
	B

func main() {
	let u: U = A{}
	panic(u)
}
`
	expectPanicAt(test, src, "prog.gol:11")
}

// expectPanicAt runs src as prog.gol and checks that its stack trace
// contains location.
func expectPanicAt(test *testing.T, src, location string) {
	test.Helper()
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
	if !strings.Contains(string(got), location) {
		test.Errorf("stack trace does not point at %s:\n%s\ngenerated:\n%s", location, got, outputs[0].Src)
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

func grow(outer box) box {
	return box{ // copy outer
		...outer,
		// a bigger size
		size: outer.size + 1, // add one
	}
}

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

// TestDestructureTempPackageName checks that the temporary of a
// destructuring does not shadow a package-level name from another file.
func TestDestructureTempPackageName(test *testing.T) {
	declarations := `package p

type tmp struct{ code int }

type point struct{ X, Y int }

func load() point { return point{1, 2} }

func each(visit func(tmp)) { visit(tmp{3}) }
`
	uses := `package p

func sum() int {
	const {X, Y} = load()
	each(item => println(item.code))
	return X + Y
}
`
	inputs := []Input{{Path: "declarations.gol", Src: []byte(declarations)}, {Path: "uses.gol", Src: []byte(uses)}}
	outputs, err := Transpile(Config{Importer: importer.Default()}, inputs)
	if err != nil {
		test.Fatal(err)
	}
	typeCheckOutputs(test, outputs)
}

// typeCheckOutputs fails the test if the generated files of one package do
// not type-check together.
func typeCheckOutputs(test *testing.T, outputs []Output) {
	test.Helper()
	fileSet := token.NewFileSet()
	var parsedFiles []*ast.File
	var sources []string
	for _, output := range outputs {
		parsedFile, err := parser.ParseFile(fileSet, output.Path, output.Src, 0)
		if err != nil {
			test.Fatal(err)
		}
		parsedFiles = append(parsedFiles, parsedFile)
		sources = append(sources, string(output.Src))
	}
	var typeErrors []string
	checkConfig := types.Config{Importer: importer.Default(), Error: func(err error) { typeErrors = append(typeErrors, err.Error()) }}
	if _, err := checkConfig.Check("p", fileSet, parsedFiles, nil); err != nil {
		test.Fatalf("generated code does not type-check:\n%s\n%s", strings.Join(typeErrors, "\n"), strings.Join(sources, "\n"))
	}
}

// TestLineDirectivesAfterMultilinePattern checks that a destructuring
// pattern over several lines does not shift the lines after it.
func TestLineDirectivesAfterMultilinePattern(test *testing.T) {
	src := `package p

type Config struct {
	Name string
	Port int
}

func load() []int { return nil }

func f(cfg Config) int {
	const {
		Name,
		Port: port,
	} = cfg
	let [
		first,
		...rest,
	] = load()
	return marker(len(Name) + port + first + len(rest))
}

func marker(n int) int { return n }
`
	outputs, err := Transpile(Config{Importer: importer.Default(), LineDirectives: true}, []Input{{Path: "p.gol", Src: []byte(src)}})
	if err != nil {
		test.Fatal(err)
	}
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "p.go", outputs[0].Src, 0)
	if err != nil {
		test.Fatal(err)
	}
	var gotLines []int
	ast.Inspect(parsed, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok && ident.Name == "marker" {
			gotLines = append(gotLines, fileSet.PositionFor(ident.Pos(), true).Line)
		}
		return true
	})
	if want := []int{19, 22}; !slices.Equal(gotLines, want) {
		test.Errorf("marker lines = %v, want %v\n%s", gotLines, want, outputs[0].Src)
	}
}

// TestNoAdjacentLineDirectives checks that the output never writes two line
// directives in a row. The second one makes the first useless.
func TestNoAdjacentLineDirectives(test *testing.T) {
	paths, err := filepath.Glob("testdata/golden/lines*.gol")
	if err != nil {
		test.Fatal(err)
	}
	adjacentDirectivePattern := regexp.MustCompile(`\*/\s*/\*line `)
	for _, path := range paths {
		if got := transpileFile(test, path, true); adjacentDirectivePattern.Match(got) {
			test.Errorf("%s has adjacent line directives:\n%s", path, got)
		}
	}
}

// TestDefaultsAcrossFiles checks a call in another file. The declaring file
// uses its time import only in a default value.
func TestDefaultsAcrossFiles(test *testing.T) {
	declaring := "package p\n\nimport \"time\"\n\nfunc Wait(d time.Duration = time.Second) time.Duration { return d }\n"
	calling := "package p\n\nfunc twice() int {\n\tlet time = 2\n\treturn int(Wait()) * time\n}\n"
	outputs, err := Transpile(Config{Importer: importer.Default()}, []Input{{Path: "a.gol", Src: []byte(declaring)}, {Path: "b.gol", Src: []byte(calling)}})
	if err != nil {
		test.Fatal(err)
	}
	if got := string(outputs[0].Src); !strings.Contains(got, "func waitDDefault() time.Duration {\n\treturn time.Second\n}") {
		test.Errorf("declaring file has no helper:\n%s", got)
	}
	if got := string(outputs[1].Src); !strings.Contains(got, "return int(Wait(waitDDefault())) * time") {
		test.Errorf("call does not use the helper:\n%s", got)
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

// TestArrowParameterNamedLikePackage checks that the parameters of an arrow
// do not hide the packages of its own types.
func TestArrowParameterNamedLikePackage(test *testing.T) {
	src := "package p\nimport \"os\"\nfunc each(f func(*os.File) *os.File) {}\nfunc g() {\n\teach(os => os)\n}\n"
	outputs, err := Transpile(Config{Importer: importer.Default()}, []Input{{Path: "x.gol", Src: []byte(src)}})
	if err != nil {
		test.Fatal(err)
	}
	if want := "func(os *os.File) *os.File"; !strings.Contains(string(outputs[0].Src), want) {
		test.Errorf("output does not contain %q:\n%s", want, outputs[0].Src)
	}
}

func TestCleanMessage(test *testing.T) {
	testCases := []struct{ message, want string }{
		{"in argument to __goslingArr_includes", "in argument to includes"},
		{"in argument to __goslingArr_findPI", "in argument to find"},
		{"__goslingArr_findIndexI(xs, f) (value of type int)", "xs.findIndex(f) (value of type int)"},
		{"__goslingArr_map(__goslingArr_filter(xs, f), g) + 1", "xs.filter(f).map(g) + 1"},
		{"x.__goslingMap undefined", "x.map undefined"},
		{"cannot use x as __goslingU value", "cannot use x as any value"},
		{"in argument to __goslingOne[int64]", "in argument to slice spread"},
		{"in argument to __goslingOne[map[string]int]", "in argument to slice spread"},
		{"in call to __goslingCat, S (type int) does not satisfy ~[]E", "in call to slice spread, S (type int) does not satisfy ~[]E"},
		{"__goslingCat(xs, __goslingOne(1, f(a, b)), ys) + 1", "[...xs, 1, f(a, b), ...ys] + 1"},
		{"__goslingT(`a (`, n, `) b`) + 1", "t`a (${n}) b` + 1"},
		{"__goslingT(`x`, …", "t(`x`, …"},
		{"__goslingV(f()) + 1", "f() + 1"},
		{"__goslingV3(f())", "f()"},
		{"__goslingNP(p, 1) + \"a\"", "(p ?? 1) + \"a\""},
		{"invalid operation: -__goslingN(p, 1)", "invalid operation: -(p ?? 1)"},
		{"__goslingN(p, q).X undefined", "(p ?? q).X undefined"},
		{"cannot use __goslingNP(p, 1) (value of type int)", "cannot use p ?? 1 (value of type int)"},
		{"f(__goslingN(p, 1), 2)", "f(p ?? 1, 2)"},
		{"in argument to __goslingN", "in argument to ??"},
		{"p.__goslingO.X undefined", "p?.X undefined"},
		{"__goslingVariants_Shape redeclared", "Shape redeclared"},
		{"in call to __goslingWith, V (type int) does not satisfy", "in call to struct spread, V (type int) does not satisfy"},
		{"my__goslingT stays", "my__goslingT stays"},
		{
			"in call to __goslingArr_mapI, type func(x int, i int) of (func(x int, i int) literal) does not match inferred type func(int, int) R for func(T, int) R",
			"map callback must return a value",
		},
		{
			"in call to __goslingArr_filter, type func(x int) (int, error) of f does not match inferred type func(int) bool for func(T) bool",
			"filter callback has type func(x int) (int, error), want func(int) bool",
		},
	}
	for _, testCase := range testCases {
		if got := cleanMessage(testCase.message); got != testCase.want {
			test.Errorf("cleanMessage(%q) = %q, want %q", testCase.message, got, testCase.want)
		}
	}
}

func TestShadowedBuiltinInOtherFile(test *testing.T) {
	inputs := []Input{
		{Path: "a.gol", Src: []byte("package p\nfunc f(xs []int) []int {\n\treturn xs.map(x => x * 2)\n}\n")},
		{Path: "b.gol", Src: []byte("package p\nfunc make(n int) int { return n }\n")},
	}
	_, err := Transpile(Config{Importer: importer.Default()}, inputs)
	if err == nil {
		test.Fatal("expected error")
	}
	want := "a.gol:3:9: map needs the builtin make, but a declaration shadows it here; rename that declaration"
	if !strings.Contains(err.Error(), want) {
		test.Errorf("error:\n%v\nwant substring:\n%s", err, want)
	}
}

func TestBuiltinDeclaredAfterUse(test *testing.T) {
	src := "package p\nfunc f(xs []int) []int {\n\tlet ys = xs.map(x => x * 2)\n\tlet len = 3\n\t_ = len\n\treturn ys\n}\n"
	if _, err := Transpile(Config{Importer: importer.Default()}, []Input{{Path: "x.gol", Src: []byte(src)}}); err != nil {
		test.Fatal(err)
	}
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

// TestKeepGoingGenericUnionSwitch checks switches on broken generic unions.
// KeepGoing lowers despite the type errors, so the exhaustive check sees
// variants that it cannot instantiate.
func TestKeepGoingGenericUnionSwitch(test *testing.T) {
	testCases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "pointer to an unnamed type",
			src:  "package p\ntype A[T any] struct{}\ntype Q[T any] = struct{ x T }\ntype U[T any] = A[T] | *Q[T]\nfunc f(u U[int]) {\n\tswitch u.(type) {\n\tcase A[int]:\n\t}\n}",
			want: "x.gol:6:2: cannot check that the switch on U[int] covers *Q[T]: *Q[T] is not a named type",
		},
		{
			name: "variant with the wrong type argument count",
			src:  "package p\ntype A[T any] struct{}\ntype B[T, V any] struct{}\ntype U[T any] = A[T] | B[T]\nfunc f(u U[int]) {\n\tswitch u.(type) {\n\tcase A[int]:\n\t}\n}",
			want: "x.gol:6:2: cannot check that the switch on U[int] covers B[T]: got 1 type arguments but B[T, V any] has 2 type parameters",
		},
	}
	for _, testCase := range testCases {
		test.Run(testCase.name, func(test *testing.T) {
			_, err := Transpile(Config{Importer: importer.Default(), KeepGoing: true}, []Input{{Path: "x.gol", Src: []byte(testCase.src)}})
			var errorList scanner.ErrorList
			if !errors.As(err, &errorList) {
				test.Fatalf("got %v, want a list of errors", err)
			}
			var messages strings.Builder
			for _, listedError := range errorList {
				messages.WriteString(listedError.Error() + "\n")
			}
			if !strings.Contains(messages.String(), testCase.want) || strings.Contains(messages.String(), "missing cases") {
				test.Errorf("errors:\n%s\nwant substring:\n%s", messages.String(), testCase.want)
			}
		})
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

// TestTightSpacing checks that gosling syntax without spaces gives the same
// output as with spaces.
func TestTightSpacing(test *testing.T) {
	const header = "package p\n\ntype U struct{ Name *string }\n\ntype Point struct{ X, Y int }\n\n"
	for _, testCase := range []struct {
		signature, spaced, tight string
	}{
		{"func f() func(int) int", "x => x * 2", "x=>x*2"},
		{"func f() func(int, int) int", "(a, b) => a + b", "(a,b)=>a+b"},
		{"func f() func(int) int", "(a int) => a + 1", "(a int)=>a+1"},
		{"func f() func(int) int", "x => { return x }", "x=>{ return x }"},
		{"func f(p *int, q int) int", "p ?? q", "p??q"},
		{"func f(u *U) string", "u?.Name ?? \"\"", "u?.Name??\"\""},
		{"func f(xs, ys []int) []int", "[...xs, ...ys]", "[...xs,...ys]"},
		{"func f(pt Point) Point", "Point{...pt, X: 1}", "Point{...pt,X:1}"},
	} {
		var outputs [2]string
		for index, body := range []string{testCase.spaced, testCase.tight} {
			src := header + testCase.signature + " {\n\treturn " + body + "\n}\n"
			transpiled, err := Transpile(Config{Importer: importer.Default()}, []Input{{Path: "p.gol", Src: []byte(src)}})
			if err != nil {
				test.Fatalf("%q: %v", body, err)
			}
			outputs[index] = string(transpiled[0].Src)
		}
		if outputs[0] != outputs[1] {
			test.Errorf("%q gives\n%s\nwant\n%s", testCase.tight, outputs[1], outputs[0])
		}
	}
}

// TestTightControlFlow checks that control flow without spaces gives the
// same output as with spaces.
func TestTightControlFlow(test *testing.T) {
	const header = "package p\n\nimport \"os\"\n\nfunc f(xs []int, m map[string]int, ok bool) int {\n\tlet n = 0\n\t_ = os.Args\n"
	for _, testCase := range []struct {
		spaced, tight string
	}{
		{"for (const x of xs) {\n\t\tn += x\n\t}", "for(const x of xs){\n\t\tn += x\n\t}"},
		{"for (let k in m) {\n\t\tn += m[k]\n\t}", "for(let k in m){\n\t\tn += m[k]\n\t}"},
		{"for (let i = 0; i < 3; i++) {\n\t\tn += i\n\t}", "for(let i = 0; i < 3; i++){\n\t\tn += i\n\t}"},
		{"if (ok) {\n\t\tn++\n\t}", "if(ok){\n\t\tn++\n\t}"},
		{"switch (n) {\n\tcase 1:\n\t\tn++\n\t}", "switch(n){\n\tcase 1:\n\t\tn++\n\t}"},
		{"try {\n\t\tos.Remove(\"x\")\n\t} catch (e) {\n\t\tprintln(e)\n\t} finally {\n\t\tn++\n\t}", "try{\n\t\tos.Remove(\"x\")\n\t}catch(e){\n\t\tprintln(e)\n\t}finally{\n\t\tn++\n\t}"},
		{"try {\n\t\tos.Remove(\"x\")\n\t} catch {\n\t\tn++\n\t}", "try{\n\t\tos.Remove(\"x\")\n\t}catch{\n\t\tn++\n\t}"},
	} {
		var outputs [2]string
		for index, body := range []string{testCase.spaced, testCase.tight} {
			src := header + "\t" + body + "\n\treturn n\n}\n"
			transpiled, err := Transpile(Config{Importer: importer.Default()}, []Input{{Path: "p.gol", Src: []byte(src)}})
			if err != nil {
				test.Errorf("%q: %v", body, err)
				continue
			}
			outputs[index] = string(transpiled[0].Src)
		}
		if outputs[0] != outputs[1] {
			test.Errorf("%q gives\n%s\nwant\n%s", testCase.tight, outputs[1], outputs[0])
		}
	}
}

func TestParseErrors(test *testing.T) {
	valid := "package p\n\nfunc f() int {\n\tlet x = 1\n\treturn x\n}\n"
	if err := FindParseErrors(Input{Path: "p.gol", Src: []byte(valid)}); err != nil {
		test.Errorf("valid file: %v", err)
	}
	for _, testCase := range []struct {
		src  string
		line int
	}{
		{"package p\n\nfunc f() int {\n\tlet x = \n}\n", 5},
		{"package p\n\nfunc f() {\n\tx := 1\n\t_ = x\n}\n", 4},
		{"package p\n\nfunc f() {\n\ttry {\n\t}\n}\n", 5},
	} {
		err := FindParseErrors(Input{Path: "p.gol", Src: []byte(testCase.src)})
		var errorList scanner.ErrorList
		if !errors.As(err, &errorList) || errorList[0].Pos.Filename != "p.gol" || errorList[0].Pos.Line != testCase.line {
			test.Errorf("FindParseErrors(%q) = %v, want an error on line %d", testCase.src, err, testCase.line)
		}
	}
}
