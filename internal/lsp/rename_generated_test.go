package lsp

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"log"
	"slices"
	"strings"
	"testing"

	"github.com/zacharyhill/gosling/internal/transpile"
)

const renameSource = `package p

import "strconv"

type Point struct{ X, Y int }

func parse(text string) int {
	try {
		const count = strconv.Atoi(text)
		return count
	} catch (e) {
		println(e.Error())
	}
	try {
		const other = strconv.Atoi(text)
		return other
	} catch (e) {
		println(e.Error())
	}
	return 0
}

func double(xs []int) []int {
	return xs.map(x => x * 2)
}

func sum(xs []int) int {
	let total = 0
	for (const value of xs) {
		total += value
	}
	return total
}

func coords(p Point) int {
	const {X: across, Y} = p
	return across + Y
}
`

// generatorFor transpiles one .gol file in memory, as the gosling command
// does for a package.
type generatorFor struct {
	golPath string
}

func (generator generatorFor) Generate(dir string, sources map[string][]byte, lineDirectives bool) ([]transpile.Output, error) {
	config := transpile.Config{Importer: importer.Default(), LineDirectives: lineDirectives}
	return transpile.Transpile(config, []transpile.Input{{Path: generator.golPath, Src: sources[generator.golPath]}})
}

func (generatorFor) Write(string) error { return nil }

// newRenameServer returns a server with the open document x.gol that holds
// source, and a generator for it.
func newRenameServer(test *testing.T, source string) (*server, *generatedFile) {
	test.Helper()
	golPath := "/work/p/x.gol"
	outputs, err := transpile.Transpile(transpile.Config{Importer: importer.Default()}, []transpile.Input{{Path: golPath, Src: []byte(source)}})
	if err != nil {
		test.Fatal(err)
	}
	server := newServer(Options{Generator: generatorFor{golPath: golPath}, Logger: log.New(io.Discard, "", 0)}, io.Discard, io.Discard)
	file := newGeneratedFile(outputs[0], []byte(source))
	server.documents[golPath] = &editorDocument{path: golPath, text: []byte(source)}
	packageState := newPackageState("/work/p")
	packageState.dirty = false
	packageState.files[file.goPath] = file
	server.packages[packageState.dir] = packageState
	return server, file
}

// goRenameEdits renames the identifier at goOffset in the generated file
// with go/types, as gopls does. It returns the result as a gopls response.
func goRenameEdits(test *testing.T, file *generatedFile, goOffset int, newName string) json.RawMessage {
	test.Helper()
	fileSet := token.NewFileSet()
	astFile, err := parser.ParseFile(fileSet, file.goPath, file.goText, 0)
	if err != nil {
		test.Fatal(err)
	}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
	if _, err := (&types.Config{Importer: importer.Default()}).Check("p", fileSet, []*ast.File{astFile}, info); err != nil {
		test.Fatal(err)
	}
	tokenFile := fileSet.File(astFile.Pos())
	var target types.Object
	ast.Inspect(astFile, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok && tokenFile.Offset(ident.Pos()) == goOffset {
			target = info.ObjectOf(ident)
		}
		return true
	})
	if target == nil {
		test.Fatalf("no identifier at Go offset %d", goOffset)
	}
	var edits []textEdit
	ast.Inspect(astFile, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok && info.ObjectOf(ident) == target {
			start := tokenFile.Offset(ident.Pos())
			editRange := lspRange{Start: positionAt(file.goText, start), End: positionAt(file.goText, start+len(ident.Name))}
			edits = append(edits, textEdit{Range: editRange, NewText: newName})
		}
		return true
	})
	raw, err := json.Marshal(map[string]any{
		"documentChanges": []any{map[string]any{"textDocument": map[string]any{"uri": pathToURI(file.goPath), "version": 1}, "edits": edits}},
	})
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

// renameAt renames the copied identifier at an occurrence of needle, moved
// by shift bytes. It returns the .gol offsets of the mapped edits.
func renameAt(test *testing.T, needle string, occurrence, shift int, newName string) ([]int, error) {
	test.Helper()
	server, file := newRenameServer(test, renameSource)
	golOffset := nthIndex(test, renameSource, needle, occurrence) + shift
	goOffset, ok := file.sourceMap.GoOffset(golOffset)
	if !ok {
		test.Fatalf("%q is not copied", needle)
	}
	mapped, err := server.mapRename(goRenameEdits(test, file, goOffset, newName))
	if err != nil {
		return nil, err
	}
	var offsets []int
	for _, change := range mapped.DocumentChanges {
		if change.TextDocument.URI != pathToURI(file.golPath) {
			test.Fatalf("edit in %s", change.TextDocument.URI)
		}
		for _, edit := range change.Edits {
			start, ok := offsetAt([]byte(renameSource), edit.Range.Start)
			if !ok || edit.NewText != newName {
				test.Fatalf("bad edit %+v", edit)
			}
			offsets = append(offsets, start)
		}
	}
	slices.Sort(offsets)
	return offsets, nil
}

// nthIndex returns the offset of an occurrence of needle. Occurrences count
// from 0.
func nthIndex(test *testing.T, text, needle string, occurrence int) int {
	test.Helper()
	offset := -1
	for range occurrence + 1 {
		next := strings.Index(text[offset+1:], needle)
		if next < 0 {
			test.Fatalf("occurrence %d of %q not found", occurrence, needle)
		}
		offset += 1 + next
	}
	return offset
}

type occurrence struct {
	needle string
	index  int
}

func TestRenameNamesInGeneratedCode(test *testing.T) {
	for _, testCase := range []struct {
		name   string
		target occurrence
		shift  int
		want   []occurrence
	}{
		{"first catch variable", occurrence{"e.Error()", 0}, 0, []occurrence{{"e) {", 0}, {"e.Error()", 0}}},
		{"second catch variable", occurrence{"e.Error()", 1}, 0, []occurrence{{"e) {", 1}, {"e.Error()", 1}}},
		{"arrow parameter", occurrence{"x * 2", 0}, 0, []occurrence{{"x =>", 0}, {"x * 2", 0}}},
		{"for-of variable", occurrence{"+= value", 0}, 3, []occurrence{{"value of", 0}, {"value\n", 0}}},
		{"destructured name", occurrence{"across +", 0}, 0, []occurrence{{"across,", 0}, {"across +", 0}}},
		{"field in a destructuring pattern", occurrence{"X, Y int", 0}, 0, []occurrence{{"X, Y int", 0}, {"X: across", 0}}},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			got, err := renameAt(test, testCase.target.needle, testCase.target.index, testCase.shift, "renamed")
			if err != nil {
				test.Fatal(err)
			}
			var want []int
			for _, wanted := range testCase.want {
				want = append(want, nthIndex(test, renameSource, wanted.needle, wanted.index))
			}
			slices.Sort(want)
			if !slices.Equal(got, want) {
				test.Errorf("edits at %v, want %v", got, want)
			}
		})
	}
}

// TestRenameRejectsShorthandField renames a field that a destructuring
// pattern also uses as a variable name. A rename of the pattern also renames
// the variable, so the rename fails.
func TestRenameRejectsShorthandField(test *testing.T) {
	if _, err := renameAt(test, "Y int", 0, 0, "Down"); !errors.Is(err, errRenameGenerated) {
		test.Errorf("err = %v, want %v", err, errRenameGenerated)
	}
}

func TestRenameTargetOfCatchVariable(test *testing.T) {
	server, file := newRenameServer(test, renameSource)
	binding := nthIndex(test, renameSource, "e) {", 1)
	goPosition, golRange, ok := server.findRenameTarget(file, positionAt([]byte(renameSource), binding))
	if !ok {
		test.Fatal("no rename target for the catch variable")
	}
	goOffset, ok := offsetAt(file.goText, goPosition)
	if !ok {
		test.Fatal("bad Go position")
	}
	golOffset, ok := file.sourceMap.GolOffset(goOffset)
	if want := nthIndex(test, renameSource, "e.Error()", 1); !ok || golOffset != want {
		test.Errorf("target maps to %d, want the use at %d", golOffset, want)
	}
	wantStart := positionAt([]byte(renameSource), binding)
	if golRange.Start != wantStart || golRange.End != (position{Line: wantStart.Line, Character: wantStart.Character + 1}) {
		test.Errorf("range = %+v, want the catch variable at %+v", golRange, wantStart)
	}
}
