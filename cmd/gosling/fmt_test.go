package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	unformattedGol = "package lib\n\nfunc f() {\ntry {\nprintln(1+2)\n}   catch (e) {\nprintln(e)\n}\n}\n"
	formattedGol   = "package lib\n\nfunc f() {\n\ttry {\n\t\tprintln(1 + 2)\n\t} catch (e) {\n\t\tprintln(e)\n\t}\n}\n"
)

var fmtFiles = map[string]string{
	"lib/a.gol":          unformattedGol,
	"lib/b.gol":          formattedGol,
	"lib/sub/c.gol":      unformattedGol,
	"lib/testdata/d.gol": unformattedGol,
}

func readTree(test *testing.T, dir string, names ...string) map[string]string {
	test.Helper()
	contents := map[string]string{}
	for _, name := range names {
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			test.Fatal(err)
		}
		contents[name] = string(content)
	}
	return contents
}

func TestFmtWritesFiles(test *testing.T) {
	dir := enterModule(test, fmtFiles)
	var stdout, stderr bytes.Buffer
	if code := runFmt([]string{"-w", "./..."}, &stdout, &stderr); code != 0 {
		test.Fatalf("exit %d: %s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		test.Errorf("-w printed %q", stdout.String())
	}
	contents := readTree(test, dir, "lib/a.gol", "lib/b.gol", "lib/sub/c.gol", "lib/testdata/d.gol")
	for _, name := range []string{"lib/a.gol", "lib/b.gol", "lib/sub/c.gol"} {
		if contents[name] != formattedGol {
			test.Errorf("%s =\n%s\nwant\n%s", name, contents[name], formattedGol)
		}
	}
	if contents["lib/testdata/d.gol"] != unformattedGol {
		test.Errorf("fmt ./... changed a file in testdata")
	}
}

func TestFmtListsFiles(test *testing.T) {
	dir := enterModule(test, fmtFiles)
	var stdout, stderr bytes.Buffer
	if code := runFmt([]string{"-l", "lib"}, &stdout, &stderr); code != 0 {
		test.Fatalf("exit %d: %s", code, stderr.String())
	}
	if want := filepath.Join("lib", "a.gol") + "\n"; stdout.String() != want {
		test.Errorf("-l printed %q, want %q", stdout.String(), want)
	}
	if contents := readTree(test, dir, "lib/a.gol"); contents["lib/a.gol"] != unformattedGol {
		test.Errorf("-l changed a.gol")
	}
}

func TestFmtPrintsResult(test *testing.T) {
	enterModule(test, fmtFiles)
	var stdout, stderr bytes.Buffer
	if code := runFmt([]string{filepath.Join("lib", "a.gol")}, &stdout, &stderr); code != 0 {
		test.Fatalf("exit %d: %s", code, stderr.String())
	}
	if stdout.String() != formattedGol {
		test.Errorf("printed\n%s\nwant\n%s", stdout.String(), formattedGol)
	}
}

func TestFmtReportsSyntaxErrors(test *testing.T) {
	enterModule(test, map[string]string{"bad.gol": "package lib\n\nfunc f() {\n\tlet x =\n}\n", "good.gol": unformattedGol})
	var stdout, stderr bytes.Buffer
	if code := runFmt([]string{"-w", "."}, &stdout, &stderr); code != 1 {
		test.Fatalf("exit %d, want 1: %s", code, stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), "bad.gol:5:1: ") {
		test.Errorf("stderr = %q, want a position in bad.gol", stderr.String())
	}
	if contents := readTree(test, ".", "good.gol"); contents["good.gol"] != formattedGol {
		test.Errorf("an error in another file stopped the format of good.gol")
	}
}
