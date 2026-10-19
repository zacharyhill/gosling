package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const lintSource = `package main

import (
	"fmt"
	"os"
)

func main() {
	os.Remove("x")
	try {
		const data = os.ReadFile("y")
		fmt.Printf("%d\n", string(data))
	} catch (e) {
		fmt.Println(e)
	}
}
`

// TestLintHelperProcess is a fake linter for runLint. It prints its
// arguments after "--" as lines and fails.
func TestLintHelperProcess(test *testing.T) {
	if os.Getenv("GOSLING_LINT_HELPER") != "1" {
		return
	}
	arguments := os.Args
	for index, argument := range arguments {
		if argument == "--" {
			arguments = arguments[index+1:]
			break
		}
	}
	fmt.Println(strings.Join(arguments, "\n"))
	fmt.Fprintln(os.Stderr, "stderr: "+arguments[0])
	os.Exit(3)
}

// findPosition returns "path:line:column" of the first occurrence of
// needle in text, with 1-based line and column.
func findPosition(path, text, needle string) string {
	offset := strings.Index(text, needle)
	lineStart := strings.LastIndex(text[:offset], "\n") + 1
	return fmt.Sprintf("%s:%d:%d", path, strings.Count(text[:offset], "\n")+1, offset-lineStart+1)
}

func runFakeLinter(test *testing.T, lines ...string) (string, string, int) {
	test.Helper()
	test.Setenv("GOSLING_LINT_HELPER", "1")
	var stdout, stderr bytes.Buffer
	args := append([]string{os.Args[0], "-test.run=TestLintHelperProcess", "--"}, lines...)
	code := runLint(args, &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func TestLintMapsPositions(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	dir := enterModule(test, map[string]string{"go.mod": goModFile, "main.gol": lintSource})
	if code := run([]string{"."}, io.Discard, io.Discard); code != 0 {
		test.Fatalf("generate: exit %d", code)
	}
	generated, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		test.Fatal(err)
	}
	removeAt := findPosition("main.go", string(generated), "os.Remove")
	printfArgument := findPosition("main.go", string(generated), "string(data)")
	removeLine, _, _ := strings.Cut(strings.TrimPrefix(removeAt, "main.go:"), ":")
	stdout, stderr, code := runFakeLinter(test,
		removeAt+": Error return value of `os.Remove` is not checked (errcheck)",
		printfArgument+": printf: wrong type (govet)",
		"main.go:"+removeLine+": a position without a column",
		"other.go:3:1: not a generated file",
	)
	if code != 3 {
		test.Errorf("exit %d, want the exit code of the linter", code)
	}
	wantLines := []string{
		findPosition("main.gol", lintSource, "os.Remove") + ": Error return value of `os.Remove` is not checked (errcheck)",
		findPosition("main.gol", lintSource, "string(data)") + ": printf: wrong type (govet)",
		findPosition("main.gol", lintSource, "os.Remove") + ": a position without a column",
		"other.go:3:1: not a generated file",
	}
	if want := strings.Join(wantLines, "\n") + "\n"; stdout != want {
		test.Errorf("stdout =\n%s\nwant\n%s", stdout, want)
	}
	if want := "stderr: " + wantLines[0] + "\n"; stderr != want {
		test.Errorf("stderr = %q, want %q", stderr, want)
	}
}

func TestLintKeepsPositionsOfStaleFiles(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	dir := enterModule(test, map[string]string{"go.mod": goModFile, "main.gol": lintSource})
	if code := run([]string{"."}, io.Discard, io.Discard); code != 0 {
		test.Fatalf("generate: exit %d", code)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.gol"), []byte(strings.Replace(lintSource, "func main", "// A new line.\nfunc main", 1)), 0o644); err != nil {
		test.Fatal(err)
	}
	stdout, stderr, _ := runFakeLinter(test, "main.go:9:2: message")
	if stdout != "main.go:9:2: message\n" {
		test.Errorf("stdout = %q, want the position unchanged", stdout)
	}
	if !strings.Contains(stderr, "gosling lint: main.go: out of date; run gosling") {
		test.Errorf("stderr = %q, want a warning", stderr)
	}
}

func TestLintWarnsAboutUnreadableFiles(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	dir := enterModule(test, map[string]string{"go.mod": goModFile, "main.gol": lintSource})
	if code := run([]string{"."}, io.Discard, io.Discard); code != 0 {
		test.Fatalf("generate: exit %d", code)
	}
	generatedPath := filepath.Join(dir, "main.go")
	if err := os.Chmod(generatedPath, 0); err != nil {
		test.Fatal(err)
	}
	if _, err := os.ReadFile(generatedPath); err == nil {
		test.Skip("file permissions do not apply to this user")
	}
	stdout, stderr, code := runFakeLinter(test, "main.go:9:2: message", "main.go:10:2: message")
	if code != 3 {
		test.Errorf("exit %d, want the exit code of the linter", code)
	}
	if stdout != "main.go:9:2: message\nmain.go:10:2: message\n" {
		test.Errorf("stdout = %q, want the positions unchanged", stdout)
	}
	if strings.Count(stderr, "gosling lint: main.go: ") != 1 || !strings.Contains(stderr, "permission denied") {
		test.Errorf("stderr = %q, want one warning that names main.go and the error", stderr)
	}
}

func TestLintWarnsWithoutWorkingDirectory(test *testing.T) {
	dir := filepath.Join(test.TempDir(), "removed")
	if err := os.Mkdir(dir, 0o755); err != nil {
		test.Fatal(err)
	}
	test.Chdir(dir)
	if err := os.Remove(dir); err != nil {
		test.Fatal(err)
	}
	if _, err := os.Getwd(); err == nil {
		test.Skip("this system finds a removed working directory")
	}
	stdout, stderr, code := runFakeLinter(test, "main.go:9:2: message")
	if code != 3 || stdout != "main.go:9:2: message\n" {
		test.Errorf("exit %d, stdout %q, want the output of the linter", code, stdout)
	}
	if !strings.Contains(stderr, "gosling lint: positions stay in .go files:") {
		test.Errorf("stderr = %q, want a warning", stderr)
	}
}

func TestLintNeedsCommand(test *testing.T) {
	var stderr bytes.Buffer
	if code := runLint(nil, io.Discard, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage: gosling lint") {
		test.Errorf("exit %d, stderr %q, want usage", code, stderr.String())
	}
}
