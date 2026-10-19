package golfmt

import (
	"bytes"
	"go/importer"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zacharyhill/gosling/internal/transpile"
)

func TestSource(test *testing.T) {
	for _, testCase := range []struct {
		name, src, want string
	}{
		{
			name: "indentation",
			src:  "package p\n\nfunc f() {\nlet x = 1\n        if x > 0 {\nprintln(x)\n    }\n}\n",
			want: "package p\n\nfunc f() {\n\tlet x = 1\n\tif x > 0 {\n\t\tprintln(x)\n\t}\n}\n",
		},
		{
			name: "space between tokens",
			src:  "package p\n\nfunc f(a, b int) int {\n\treturn a+b*2\n}\n",
			want: "package p\n\nfunc f(a, b int) int {\n\treturn a + b*2\n}\n",
		},
		{
			name: "blank lines and trailing space",
			src:  "package p   \n\n\n\nfunc f() {}   \n\n\n",
			want: "package p\n\nfunc f() {}\n",
		},
		{
			name: "try and catch",
			src:  "package p\n\nimport \"os\"\n\nfunc f() {\ntry  {\nos.Remove(\"x\")\n}   catch (e) {\nprintln(e)\n}   finally {\nprintln()\n}\n}\n",
			want: "package p\n\nimport \"os\"\n\nfunc f() {\n\ttry {\n\t\tos.Remove(\"x\")\n\t} catch (e) {\n\t\tprintln(e)\n\t} finally {\n\t\tprintln()\n\t}\n}\n",
		},
		{
			name: "space inside gosling syntax stays",
			src:  "package p\n\nfunc f(xs []int, s *string) {\n\tfor (const x of xs) {\n\t\tprintln(t`${x+1}`, s?.Len ?? 0)\n\t}\n\tconst ys = xs.filter(x => x%2==0)\n\t_ = ys\n}\n",
			want: "package p\n\nfunc f(xs []int, s *string) {\n\tfor (const x of xs) {\n\t\tprintln(t`${x+1}`, s?.Len ?? 0)\n\t}\n\tconst ys = xs.filter(x => x%2 == 0)\n\t_ = ys\n}\n",
		},
		{
			name: "aligned comments after let",
			src:  "package p\n\nfunc f() {\n\tlet count = 0 // a\n\tlet names: []string // b\n\t_, _ = count, names\n}\n",
			want: "package p\n\nfunc f() {\n\tlet count = 0       // a\n\tlet names: []string // b\n\t_, _ = count, names\n}\n",
		},
		{
			name: "lone comment after try",
			src:  "package p\n\nfunc f() {\n\ttry { // a\n\t\tprintln()\n\t} catch {\n\t}\n}\n",
			want: "package p\n\nfunc f() {\n\ttry { // a\n\t\tprintln()\n\t} catch {\n\t}\n}\n",
		},
		{
			name: "struct fields align",
			src:  "package p\n\ntype T struct {\n\tName string\n\tPort int `json:\"port\"`\n\tVeryLongName bool\n}\n",
			want: "package p\n\ntype T struct {\n\tName         string\n\tPort         int `json:\"port\"`\n\tVeryLongName bool\n}\n",
		},
		{
			name: "struct spread with comments",
			src:  "package p\n\nfunc f(u User) User {\nreturn User{...u, // base\n// age\nage: 1,\ntags: nil, // last\n// before close\n}\n}\n",
			want: "package p\n\nfunc f(u User) User {\n\treturn User{...u, // base\n\t\t// age\n\t\tage: 1,\n\t\ttags: nil, // last\n\t\t// before close\n\t}\n}\n",
		},
		{
			name: "multi-line destructuring",
			src:  "package p\n\nfunc f(c Config, xs []int) {\nconst {\nName: name,\nPort,\n} = c\nlet [\nfirst,\n...rest\n] = xs\nprintln(name, Port, first, rest)\n}\n",
			want: "package p\n\nfunc f(c Config, xs []int) {\n\tconst {\n\t\tName: name,\n\t\tPort,\n\t} = c\n\tlet [\n\t\tfirst,\n\t\t...rest\n\t] = xs\n\tprintln(name, Port, first, rest)\n}\n",
		},
		{
			name: "regex literals keep their text",
			src:  "package p\n\nfunc f(s string) bool {\nreturn /a  b\"/.MatchString(s)&&  /x/i.MatchString(s)\n}\n",
			want: "package p\n\nfunc f(s string) bool {\n\treturn /a  b\"/.MatchString(s) && /x/i.MatchString(s)\n}\n",
		},
		{
			name: "space before regex literal after define",
			src:  "package p\n\nfunc f() {\n\tx:=/a+/\n\t_ = x\n}\n",
			want: "package p\n\nfunc f() {\n\tx := /a+/\n\t_ = x\n}\n",
		},
		{
			name: "space before regex literal after return",
			src:  "package p\n\nimport \"regexp\"\n\nfunc f() *regexp.Regexp {\n\treturn/a  b/i\n}\n",
			want: "package p\n\nimport \"regexp\"\n\nfunc f() *regexp.Regexp {\n\treturn /a  b/i\n}\n",
		},
		{
			name: "space around regex literal in call arguments",
			src:  "package p\n\nfunc f(s string) {\n\tg(s,/a/,/b/)\n\tg(/d/)\n\tconst r=/c/\n\t_ = r\n}\n",
			want: "package p\n\nfunc f(s string) {\n\tg(s, /a/, /b/)\n\tg(/d/)\n\tconst r = /c/\n\t_ = r\n}\n",
		},
		{
			name: "nullish assignment",
			src:  "package p\n\nfunc f(p *int, q *int) {\np ??= q\n}\n",
			want: "package p\n\nfunc f(p *int, q *int) {\n\tp ??= q\n}\n",
		},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			got, err := Source([]byte(testCase.src))
			if err != nil {
				test.Fatal(err)
			}
			if string(got) != testCase.want {
				test.Errorf("got\n%s\nwant\n%s", got, testCase.want)
			}
			again, err := Source(got)
			if err != nil {
				test.Fatal(err)
			}
			if !bytes.Equal(again, got) {
				test.Errorf("second format differs:\n%s", again)
			}
		})
	}
}

func TestSourceErrors(test *testing.T) {
	for _, src := range []string{
		"package p\n\nfunc f() {\n\tlet x = \n}\n",
		"package p\n\nfunc f() {\n\ttry {\n\t}\n}\n",
	} {
		if got, err := Source([]byte(src)); err == nil {
			test.Errorf("Source(%q) = %q, want an error", src, got)
		}
	}
}

func TestEdits(test *testing.T) {
	src := "package p\n\nfunc f() {\nprintln(1)\n\tprintln(2)\n}\n"
	edits, err := Edits([]byte(src))
	if err != nil {
		test.Fatal(err)
	}
	insertAt := strings.Index(src, "println(1)")
	want := Edit{Start: insertAt, End: insertAt, Text: "\t"}
	if len(edits) != 1 || edits[0] != want {
		test.Errorf("edits = %+v, want only %+v", edits, want)
	}
}

// TestRepositoryFiles formats every .gol file in the repository. The result
// must be stable and must transpile to the same Go code. The file without
// indentation must format to the same result.
func TestRepositoryFiles(test *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		test.Fatal(err)
	}
	var paths []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == "node_modules" || strings.HasPrefix(entry.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".gol") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
	if len(paths) == 0 {
		test.Fatal("found no .gol files")
	}
	for _, path := range paths {
		relativePath, err := filepath.Rel(root, path)
		if err != nil {
			test.Fatal(err)
		}
		test.Run(relativePath, func(test *testing.T) {
			checkFormatPreservesOutput(test, path)
		})
	}
}

// lineIndentation matches the indentation of each line. Removing it can
// change a multi-line raw string, so the test first compares the tokens.
var lineIndentation = regexp.MustCompile(`(?m)^[ \t]+`)

func checkFormatPreservesOutput(test *testing.T, path string) {
	src, err := os.ReadFile(path)
	if err != nil {
		test.Fatal(err)
	}
	formatted, err := Source(src)
	if err != nil {
		test.Fatal(err)
	}
	again, err := Source(formatted)
	if err != nil {
		test.Fatal(err)
	}
	if !bytes.Equal(again, formatted) {
		test.Errorf("formatting is not stable:\n%s", again)
	}
	unindented := lineIndentation.ReplaceAll(src, nil)
	if equalTokens(scanTokens(src, true), scanTokens(unindented, true)) {
		reindented, err := Source(unindented)
		if err != nil {
			test.Fatal(err)
		}
		if !bytes.Equal(reindented, formatted) {
			test.Errorf("the file without indentation formats differently:\n%s", reindented)
		}
	}
	before := transpileSource(test, path, src)
	after := transpileSource(test, path, formatted)
	if !bytes.Equal(before, after) {
		test.Errorf("formatting changed the generated code:\n%s\nformatted source:\n%s", after, formatted)
	}
}

func transpileSource(test *testing.T, path string, src []byte) []byte {
	test.Helper()
	outputs, err := transpile.Transpile(transpile.Config{Importer: importer.Default()}, []transpile.Input{{Path: path, Src: src}})
	if err != nil {
		test.Fatalf("Transpile: %v", err)
	}
	return outputs[0].Src
}
