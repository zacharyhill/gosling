package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var constrainedFiles = map[string]string{
	"go.mod": goModFile,
	"plat/plat.gol": `package plat

func Name() string {
	return platformName()
}
`,
	"plat/plat_linux.gol": `package plat

func platformName() string {
	return "linux"
}
`,
	"plat/plat_windows.gol": `package plat

import "syscall"

func platformName() string {
	const handle = syscall.InvalidHandle
	return t` + "`windows ${handle} ${windowsOnly()}`" + `
}
`,
	"plat/helper_windows.go": `package plat

func windowsOnly() int { return 1 }
`,
	"plat/plat_other.gol": `//go:build !linux && !windows

package plat

func platformName() string {
	return "other"
}
`,
	"plat/extra.gol": `//go:build integration

package plat

func Extra() string {
	return "extra " + platformName()
}
`,
	"plat/gen.gol": `//go:build ignore

package main

import "fmt"

func main() {
	fmt.Println(t` + "`generated`" + `)
}
`,
}

func TestBuildConstraints(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	dir := enterModule(test, constrainedFiles)
	var stderr bytes.Buffer
	if code := run([]string{"./..."}, io.Discard, &stderr); code != 0 {
		test.Fatalf("exit %d: %s", code, stderr.String())
	}
	for name, constraint := range map[string]string{
		"plat.go":         "",
		"plat_linux.go":   "",
		"plat_windows.go": "",
		"plat_other.go":   "//go:build !linux && !windows\n",
		"extra.go":        "//go:build integration\n",
		"gen.go":          "//go:build ignore\n",
	} {
		generated, err := os.ReadFile(filepath.Join(dir, "plat", name))
		if err != nil {
			test.Fatalf("missing output %s: %v", name, err)
		}
		header, _, _ := strings.Cut(string(generated), "package ")
		if constraint != "" && !strings.Contains(header, constraint) {
			test.Errorf("%s does not keep its constraint before the package clause:\n%s", name, generated)
		}
	}
	for _, environment := range [][]string{
		{"vet", "./..."},
		{"vet", "-tags", "integration", "./..."},
		{"GOOS=windows", "vet", "./..."},
		{"GOOS=darwin", "vet", "./..."},
		{"run", "plat/gen.go"},
	} {
		command := exec.Command("go")
		for _, argument := range environment {
			if goos, ok := strings.CutPrefix(argument, "GOOS="); ok {
				command.Env = append(os.Environ(), "GOOS="+goos)
				continue
			}
			command.Args = append(command.Args, argument)
		}
		if output, err := command.CombinedOutput(); err != nil {
			test.Errorf("%v: %v\n%s", environment, err, output)
		}
	}
	stderr.Reset()
	if code := run([]string{"-check", "./..."}, io.Discard, &stderr); code != 0 {
		test.Errorf("check after generate: exit %d: %s", code, stderr.String())
	}
}

func TestBuildConstraintErrors(test *testing.T) {
	if testing.Short() {
		test.Skip("runs the go tool")
	}
	for _, testCase := range []struct {
		name, file, src, want string
	}{
		{
			name: "type error in a file for another system",
			file: "plat/plat_windows.gol",
			src:  "package plat\n\nfunc platformName() string {\n\treturn undefinedName\n}\n",
			want: "plat_windows.gol:4:9: undefined: undefinedName",
		},
		{
			name: "constraint that no system meets",
			file: "plat/never.gol",
			src:  "//go:build linux && windows\n\npackage plat\n",
			want: "never.gol: no build configuration matches its build constraints",
		},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			files := map[string]string{}
			for name, content := range constrainedFiles {
				files[name] = content
			}
			files[testCase.file] = testCase.src
			enterModule(test, files)
			var stderr bytes.Buffer
			if code := run([]string{"./..."}, io.Discard, &stderr); code != 1 {
				test.Fatalf("exit %d, want 1: %s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), testCase.want) {
				test.Errorf("stderr = %q, want %q", stderr.String(), testCase.want)
			}
		})
	}
}
