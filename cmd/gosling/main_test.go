package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zacharyhill/gosling/internal/transpile"
)

func writeTree(test *testing.T, root string, files map[string]string) {
	test.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			test.Fatal(err)
		}
	}
}

// enterModule writes files to a new temporary directory and makes it the
// working directory.
func enterModule(test *testing.T, files map[string]string) string {
	test.Helper()
	dir := test.TempDir()
	writeTree(test, dir, files)
	test.Chdir(dir)
	return dir
}

const goModFile = "module example.com/app\n\ngo 1.26\n"

var moduleFiles = map[string]string{
	"go.mod": goModFile,
	"lib/lib.gol": `package lib

import "strconv"

// Double parses s and doubles it.
func Double(s string) (int, error) {
	try {
		const parsed = strconv.Atoi(s)
		return parsed * 2, nil
	} catch (e) {
		return 0, e
	}
}
`,
	"lib/helper.go": `package lib

func Triple(n int) int { return n * 3 }
`,
	"lib/lib_test.gol": `package lib

import "testing"

func TestDouble(t *testing.T) {
	try {
		const doubled = Double("21")
		if doubled != 42 {
			t.Fatalf("got %d", doubled)
		}
	} catch (e) {
		t.Fatal(e)
	}
}
`,
	"lib/ext_test.gol": `package lib_test

import (
	"testing"

	"example.com/app/lib"
)

func TestTriple(t *testing.T) {
	try {
		const doubled = lib.Double("2")
		if lib.Triple(doubled) != 12 {
			t.Fatal("bad")
		}
	} catch (e) {
		t.Fatal(e)
	}
}
`,
	"main.gol": `package main

import (
	"fmt"

	"example.com/app/lib"
)

func main() {
	try {
		const doubled = lib.Double("5")
		fmt.Println(doubled)
		lib.Double("nope")
		fmt.Println("unreachable")
	} catch (e) {
		fmt.Println("error:", e)
	}
}
`,
}

func TestRunModule(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	dir := enterModule(test, moduleFiles)

	var stderr bytes.Buffer
	if code := run([]string{"./..."}, &stderr); code != 0 {
		test.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, name := range []string{"main.go", "lib/lib.go", "lib/lib_test.go", "lib/ext_test.go"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			test.Errorf("missing output %s: %v", name, err)
		}
	}

	output, err := exec.Command("go", "run", ".").CombinedOutput()
	if err != nil {
		test.Fatalf("go run: %v\n%s", err, output)
	}
	want := "10\nerror: strconv.Atoi: parsing \"nope\": invalid syntax\n"
	if string(output) != want {
		test.Errorf("output %q, want %q", output, want)
	}
	if output, err := exec.Command("go", "test", "./...").CombinedOutput(); err != nil {
		test.Fatalf("go test: %v\n%s", err, output)
	}

	stderr.Reset()
	if code := run([]string{"-check", "./..."}, &stderr); code != 0 {
		test.Errorf("check after generate: exit %d: %s", code, stderr.String())
	}
	for _, want := range []string{"checking lib (1/2)\n", "checking . (2/2)\n", "4 generated files up to date\n"} {
		if !strings.Contains(stderr.String(), want) {
			test.Errorf("check progress missing %q: %s", want, stderr.String())
		}
	}
}

func TestVerboseGenerate(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	enterModule(test, moduleFiles)

	var stderr bytes.Buffer
	if code := run([]string{"./..."}, &stderr); code != 0 {
		test.Fatalf("exit %d: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		test.Errorf("generate without -v wrote output: %s", stderr.String())
	}

	stderr.Reset()
	if code := run([]string{"-v", "./..."}, &stderr); code != 0 {
		test.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, want := range []string{"generating lib (1/2)\n", "generating . (2/2)\n", "wrote 0 generated files, 4 unchanged\n"} {
		if !strings.Contains(stderr.String(), want) {
			test.Errorf("verbose output missing %q: %s", want, stderr.String())
		}
	}
}

func TestNoArgsHintsRecursive(test *testing.T) {
	enterModule(test, map[string]string{
		"go.mod":      goModFile,
		"lib/lib.gol": "package lib\n",
	})
	var stderr bytes.Buffer
	if code := run(nil, &stderr); code != 1 {
		test.Fatalf("exit %d, want 1: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "gosling ./...") {
		test.Errorf("stderr: %s", stderr.String())
	}
}

func TestCheckReportsStale(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	dir := enterModule(test, map[string]string{
		"go.mod":   goModFile,
		"main.gol": "package main\n\nimport \"os\"\n\nfunc main() {\n\ttry {\n\t\tos.Remove(\"x\")\n\t} catch {\n\t}\n}\n",
	})
	var stderr bytes.Buffer
	if code := run([]string{"-check", "."}, &stderr); code != 1 {
		test.Fatalf("exit %d, want 1: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "main.go is out of date") {
		test.Errorf("stderr: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "main.go")); !os.IsNotExist(err) {
		test.Error("-check wrote output")
	}
}

func TestRefusesHandWrittenFile(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	dir := enterModule(test, map[string]string{
		"go.mod":   goModFile,
		"main.gol": "package main\n\nimport \"os\"\n\nfunc run() error {\n\ttry {\n\t\tos.Remove(\"x\")\n\t} catch (e) {\n\t\treturn e\n\t}\n\treturn nil\n}\n",
		"main.go":  "package main\n\nfunc main() {}\n",
	})
	var stderr bytes.Buffer
	if code := run([]string{"."}, &stderr); code != 1 {
		test.Fatalf("exit %d, want 1: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not generated by gosling") {
		test.Errorf("stderr: %s", stderr.String())
	}
	got, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		test.Fatal(err)
	}
	if string(got) != "package main\n\nfunc main() {}\n" {
		test.Error("hand-written main.go was modified")
	}
}

// renamedTree is a module after a rename of foo.gol to bar.gol. foo.go is
// the output of the old name, and it declares the same function as bar.gol.
var renamedTree = map[string]string{
	"go.mod":  goModFile,
	"bar.gol": "package main\n\nfunc helper() int { return 2 }\n\nfunc main() { println(helper(), keep()) }\n",
	"foo.go":  transpile.GeneratedPrefix + " from foo.gol. DO NOT EDIT.\n\npackage main\n\nfunc helper() int { return 1 }\n",
	"keep.go": "package main\n\nfunc keep() int { return 3 }\n",
}

func TestRemovesOrphanedGeneratedFile(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	dir := enterModule(test, renamedTree)
	var stderr bytes.Buffer
	if code := run([]string{"."}, &stderr); code != 0 {
		test.Fatalf("exit %d, want 0: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "removed foo.go") {
		test.Errorf("stderr: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "foo.go")); !os.IsNotExist(err) {
		test.Errorf("foo.go still exists: %v", err)
	}
	for _, name := range []string{"bar.go", "keep.go"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			test.Errorf("%s: %v", name, err)
		}
	}
}

func TestCheckReportsOrphanedGeneratedFile(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	dir := enterModule(test, renamedTree)
	var stderr bytes.Buffer
	if code := run([]string{"-check", "."}, &stderr); code != 1 {
		test.Fatalf("exit %d, want 1: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "foo.go has no source file") {
		test.Errorf("stderr: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "foo.go")); err != nil {
		test.Errorf("-check removed foo.go: %v", err)
	}
}

// orphanOnlyTree has a directory whose last .gol file is gone. Only the
// output of that file is left.
var orphanOnlyTree = map[string]string{
	"go.mod":     goModFile,
	"main.gol":   "package main\n\nfunc main() {}\n",
	"lib/old.go": transpile.GeneratedPrefix + " from old.gol. DO NOT EDIT.\n\npackage lib\n",
}

func TestRemovesOrphansInDirectoryWithoutSources(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	dir := enterModule(test, orphanOnlyTree)
	var stderr bytes.Buffer
	if code := run([]string{"."}, &stderr); code != 0 {
		test.Fatalf("generate main.go: exit %d: %s", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"-check", "./..."}, &stderr); code != 1 {
		test.Fatalf("-check exit %d, want 1: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), filepath.Join("lib", "old.go")+" has no source file") {
		test.Errorf("-check stderr: %s", stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"./..."}, &stderr); code != 0 {
		test.Fatalf("exit %d, want 0: %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "lib", "old.go")); !os.IsNotExist(err) {
		test.Errorf("lib/old.go still exists: %v", err)
	}
}

// TestLanguageServerIgnoresOrphanedGeneratedFile checks the editor path. It
// must not report the functions of foo.go as declared twice.
func TestLanguageServerIgnoresOrphanedGeneratedFile(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	dir := test.TempDir()
	writeTree(test, dir, renamedTree)
	outputs, err := generator{}.Generate(dir, nil, false)
	if err != nil {
		test.Fatalf("Generate: %v", err)
	}
	if len(outputs) != 1 || filepath.Base(outputs[0].Path) != "bar.go" {
		test.Errorf("outputs = %v, want only bar.go", outputs)
	}
}

func TestReportsSourceErrors(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	enterModule(test, map[string]string{
		"go.mod":   goModFile,
		"main.gol": "package main\n\nfunc main() {\n\tmissing()\n}\n",
	})
	var stderr bytes.Buffer
	if code := run([]string{"."}, &stderr); code != 1 {
		test.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "main.gol:4:2: undefined: missing") {
		test.Errorf("stderr: %s", stderr.String())
	}
}

// TestImportsTransitiveTypes checks that gosling can name and import a type
// that it reaches only through an imported package (url.URL through net/http).
func TestImportsTransitiveTypes(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	enterModule(test, map[string]string{
		"go.mod":   goModFile,
		"main.gol": "package main\n\nimport \"net/http\"\n\nfunc main() {\n\tconst requests = []*http.Request{{Method: \"GET\"}}\n\tprintln(len(requests.map(request => request.URL).map(address => address?.Path ?? \"/\")))\n}\n",
	})
	var stderr bytes.Buffer
	if code := run([]string{"."}, &stderr); code != 0 {
		test.Fatalf("exit %d: %s", code, stderr.String())
	}
	output, err := exec.Command("go", "run", ".").CombinedOutput()
	if err != nil || string(output) != "1\n" {
		test.Fatalf("go run: %v\n%s", err, output)
	}
}
