package lsp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/scanner"
	"os"
	"path/filepath"
	"strings"

	"github.com/zacharyhill/gosling/internal/transpile"
)

// Generator transpiles the .gol files of one directory.
type Generator interface {
	// Generate returns the outputs in memory. sources holds editor text that
	// replaces files on disk.
	Generate(dir string, sources map[string][]byte, lineDirectives bool) ([]transpile.Output, error)
	// Write writes the generated .go files from the files on disk, as the
	// gosling command does.
	Write(dir string) error
}

// generatedFile is one generated .go file and the .gol text it came from.
type generatedFile struct {
	golPath   string
	golText   []byte
	goPath    string
	goText    []byte
	sourceMap transpile.SourceMap
}

func newGeneratedFile(output transpile.Output, golText []byte) *generatedFile {
	return &generatedFile{
		golPath:   golPathFor(output.Path),
		golText:   golText,
		goPath:    output.Path,
		goText:    output.Src,
		sourceMap: output.SourceMap,
	}
}

const sourceExt = ".gol"

// golPathFor returns the .gol path of a generated .go path.
func golPathFor(goPath string) string {
	return strings.TrimSuffix(goPath, ".go") + sourceExt
}

// editorDocument is a .gol file that the editor has open.
type editorDocument struct {
	path string
	text []byte
}

// packageState holds the generated files of one directory with open
// documents. gopls sees these files as open documents.
type packageState struct {
	dir   string
	dirty bool
	// files maps each generated .go path to its file.
	files map[string]*generatedFile
	// goplsVersions holds the version of each file that gopls has open.
	goplsVersions map[string]int
	// problems holds gosling errors by .gol path. While a package has
	// problems, gopls diagnostics describe old code, so the server does not
	// show them.
	problems map[string][]diagnostic
	// goplsDiagnostics holds the raw gopls diagnostics by .go path.
	goplsDiagnostics map[string][]json.RawMessage
}

func newPackageState(dir string) *packageState {
	return &packageState{
		dir:              dir,
		dirty:            true,
		files:            map[string]*generatedFile{},
		goplsVersions:    map[string]int{},
		problems:         map[string][]diagnostic{},
		goplsDiagnostics: map[string][]json.RawMessage{},
	}
}

func (packageState *packageState) fileForGol(golPath string) *generatedFile {
	return packageState.files[transpile.OutputPath(golPath)]
}

// documentsIn returns the open documents in dir.
func (server *server) documentsIn(dir string) []*editorDocument {
	var found []*editorDocument
	for path, document := range server.documents {
		if filepath.Dir(path) == dir {
			found = append(found, document)
		}
	}
	return found
}

type diagnostic struct {
	Range    lspRange `json:"range"`
	Severity int      `json:"severity"`
	Source   string   `json:"source"`
	Message  string   `json:"message"`
}

const severityError = 1

// regenerate transpiles the package with the editor text of its open
// documents. A file that fails keeps its last good version, so gopls keeps
// working on the old code. A syntax error stops the whole package. So a
// document with a syntax error takes only the changed lines that parse.
func (server *server) regenerate(packageState *packageState) error {
	packageState.dirty = false
	sources := map[string][]byte{}
	for _, document := range server.documentsIn(packageState.dir) {
		sources[document.path] = document.text
	}
	parseErr := server.useParseableVersions(packageState, sources)
	outputs, err := server.generator.Generate(packageState.dir, sources, false)
	if parseErr != nil {
		// Other errors describe the parseable versions, not the editor text.
		err = parseErr
	}
	packageState.problems = map[string][]diagnostic{}
	if err != nil {
		server.recordProblems(packageState, err)
	}
	if err == nil || len(outputs) > 0 {
		newFiles := server.newGeneratedFiles(outputs, sources)
		if err != nil {
			keepPreviousFiles(newFiles, packageState.files)
		}
		if err := server.syncGopls(packageState, newFiles); err != nil {
			return err
		}
	}
	return server.publishPackageDiagnostics(packageState)
}

// useParseableVersions replaces each source with parse errors by a version
// that parses, if the document parsed before. It returns the parse errors.
func (server *server) useParseableVersions(packageState *packageState, sources map[string][]byte) error {
	var parseErrors []error
	for golPath, text := range sources {
		input := transpile.Input{Path: golPath, Src: text}
		err := transpile.FindParseErrors(input)
		if err == nil {
			continue
		}
		parseErrors = append(parseErrors, err)
		if previous := packageState.fileForGol(golPath); previous != nil {
			sources[golPath] = buildParseableVersion(input, previous.golText, err)
		}
	}
	return errors.Join(parseErrors...)
}

// keepPreviousFiles adds each previous file that newFiles does not replace.
func keepPreviousFiles(newFiles, previousFiles map[string]*generatedFile) {
	for goPath, previous := range previousFiles {
		if _, ok := newFiles[goPath]; !ok {
			newFiles[goPath] = previous
		}
	}
}

func (server *server) newGeneratedFiles(outputs []transpile.Output, sources map[string][]byte) map[string]*generatedFile {
	files := map[string]*generatedFile{}
	for _, output := range outputs {
		golPath := golPathFor(output.Path)
		golText, ok := sources[golPath]
		if !ok {
			var err error
			if golText, err = os.ReadFile(golPath); err != nil {
				server.logger.Printf("read %s: %v", golPath, err)
				continue
			}
		}
		files[output.Path] = newGeneratedFile(output, golText)
	}
	return files
}

// recordProblems turns a generation error into diagnostics. The error can
// join the errors of several groups. Internal tests type-check the package
// files again, so the same error can occur twice.
func (server *server) recordProblems(packageState *packageState, err error) {
	seen := map[string]bool{}
	for _, problem := range splitErrors(err) {
		path, problemDiagnostic := server.problemDiagnostic(packageState, problem)
		key := fmt.Sprintf("%s:%v:%s", path, problemDiagnostic.Range.Start, problemDiagnostic.Message)
		if path == "" || seen[key] {
			continue
		}
		seen[key] = true
		packageState.problems[path] = append(packageState.problems[path], problemDiagnostic)
	}
}

// splitErrors flattens joined errors and error lists. Each result is a
// *scanner.Error or an error without a position.
func splitErrors(err error) []error {
	var split []error
	switch typed := err.(type) {
	case scanner.ErrorList:
		for _, problem := range typed {
			split = append(split, problem)
		}
	case interface{ Unwrap() []error }:
		for _, inner := range typed.Unwrap() {
			split = append(split, splitErrors(inner)...)
		}
	default:
		split = append(split, err)
	}
	return split
}

// problemDiagnostic returns the .gol path and diagnostic for one error. An
// error without a position goes on the first open document of the package.
func (server *server) problemDiagnostic(packageState *packageState, problem error) (string, diagnostic) {
	golDiagnostic := diagnostic{Severity: severityError, Source: "gosling", Message: problem.Error()}
	var positioned *scanner.Error
	if !errors.As(problem, &positioned) || positioned.Pos.Filename == "" {
		if documents := server.documentsIn(packageState.dir); len(documents) > 0 {
			return documents[0].path, golDiagnostic
		}
		return "", golDiagnostic
	}
	text := server.golText(positioned.Pos.Filename)
	lineStart, ok := offsetAt(text, position{Line: positioned.Pos.Line - 1})
	if !ok {
		lineStart = 0
	}
	start := positionAt(text, lineStart+max(positioned.Pos.Column-1, 0))
	golDiagnostic.Range = lspRange{Start: start, End: start}
	golDiagnostic.Message = positioned.Msg
	return positioned.Pos.Filename, golDiagnostic
}

// golText returns the editor text of a .gol file, or the text on disk.
func (server *server) golText(path string) []byte {
	if document, ok := server.documents[path]; ok {
		return document.text
	}
	text, err := os.ReadFile(path)
	if err != nil {
		server.logger.Printf("read %s: %v", path, err)
	}
	return text
}

// syncGopls sends the new generated files to gopls as open documents, and
// closes files that the package no longer generates.
func (server *server) syncGopls(packageState *packageState, newFiles map[string]*generatedFile) error {
	for goPath := range packageState.files {
		if _, ok := newFiles[goPath]; !ok {
			if err := server.closeInGopls(packageState, goPath); err != nil {
				return err
			}
		}
	}
	for goPath, file := range newFiles {
		previous, wasOpen := packageState.files[goPath]
		packageState.files[goPath] = file
		if wasOpen && bytes.Equal(previous.goText, file.goText) {
			continue
		}
		if err := server.sendToGopls(packageState, file, wasOpen); err != nil {
			return err
		}
	}
	return nil
}

func (server *server) sendToGopls(packageState *packageState, file *generatedFile, wasOpen bool) error {
	packageState.goplsVersions[file.goPath]++
	uri := pathToURI(file.goPath)
	version := packageState.goplsVersions[file.goPath]
	if !wasOpen {
		return server.notifyGopls("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{"uri": uri, "languageId": "go", "version": version, "text": string(file.goText)},
		})
	}
	return server.notifyGopls("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": version},
		"contentChanges": []any{map[string]any{"text": string(file.goText)}},
	})
}

func (server *server) closeInGopls(packageState *packageState, goPath string) error {
	delete(packageState.files, goPath)
	delete(packageState.goplsVersions, goPath)
	delete(packageState.goplsDiagnostics, goPath)
	return server.notifyGopls("textDocument/didClose", map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(goPath)},
	})
}

// closePackage closes the generated files of a package in gopls and clears
// the diagnostics of its documents.
func (server *server) closePackage(packageState *packageState) error {
	for goPath, file := range packageState.files {
		if err := server.closeInGopls(packageState, goPath); err != nil {
			return err
		}
		if err := server.publishDiagnostics(file.golPath, nil); err != nil {
			return err
		}
	}
	delete(server.packages, packageState.dir)
	return nil
}

// publishPackageDiagnostics sends the diagnostics of each open document in
// the package.
func (server *server) publishPackageDiagnostics(packageState *packageState) error {
	for _, document := range server.documentsIn(packageState.dir) {
		if err := server.publishDiagnostics(document.path, server.documentDiagnostics(packageState, document.path)); err != nil {
			return err
		}
	}
	return nil
}

func (server *server) documentDiagnostics(packageState *packageState, golPath string) []any {
	var diagnostics []any
	if len(packageState.problems) > 0 {
		for _, problem := range packageState.problems[golPath] {
			diagnostics = append(diagnostics, problem)
		}
		return diagnostics
	}
	file := packageState.fileForGol(golPath)
	if file == nil {
		return nil
	}
	mapper := server.newMapper(file, true)
	for _, raw := range packageState.goplsDiagnostics[file.goPath] {
		if mapped, ok := mapper.mapJSON(raw); ok {
			diagnostics = append(diagnostics, mapped)
		}
	}
	return diagnostics
}

func (server *server) publishDiagnostics(golPath string, diagnostics []any) error {
	if diagnostics == nil {
		diagnostics = []any{}
	}
	return server.notifyClient("textDocument/publishDiagnostics", map[string]any{
		"uri":         pathToURI(golPath),
		"diagnostics": diagnostics,
	})
}
