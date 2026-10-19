package lsp

import (
	"encoding/json"
	"go/importer"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/zacharyhill/gosling/internal/transpile"
)

const mappingSource = "package p\n\nimport \"strconv\"\n\nfunc parse(text string) int {\n\ttry {\n\t\tconst count = strconv.Atoi(text)\n\t\treturn count\n\t} catch {\n\t\treturn 0\n\t}\n}\n"

// newMappingServer returns a server with one open document and its
// generated file, without gopls.
func newMappingServer(test *testing.T) (*server, *generatedFile) {
	test.Helper()
	return newServerFor(test, mappingSource)
}

// newServerFor returns a server with the open document x.gol that holds
// source.
func newServerFor(test *testing.T, source string) (*server, *generatedFile) {
	test.Helper()
	golPath := "/work/p/x.gol"
	outputs, err := transpile.Transpile(transpile.Config{Importer: importer.Default()}, []transpile.Input{{Path: golPath, Src: []byte(source)}})
	if err != nil {
		test.Fatal(err)
	}
	server := newServer(Options{Logger: log.New(io.Discard, "", 0)}, io.Discard, io.Discard)
	file := newGeneratedFile(outputs[0], []byte(source))
	server.documents[golPath] = &editorDocument{path: golPath, text: []byte(source)}
	packageState := newPackageState("/work/p")
	packageState.dirty = false
	packageState.files[file.goPath] = file
	server.packages[packageState.dir] = packageState
	return server, file
}

func goRange(test *testing.T, file *generatedFile, needle string) lspRange {
	test.Helper()
	offset := strings.Index(string(file.goText), needle)
	if offset < 0 {
		test.Fatalf("%q not in generated file", needle)
	}
	return lspRange{Start: positionAt(file.goText, offset), End: positionAt(file.goText, offset+len(needle))}
}

func golRange(test *testing.T, needle string) lspRange {
	test.Helper()
	offset := strings.Index(mappingSource, needle)
	return lspRange{Start: positionAt([]byte(mappingSource), offset), End: positionAt([]byte(mappingSource), offset+len(needle))}
}

func TestMapperMapsCompletionRanges(test *testing.T) {
	server, file := newMappingServer(test)
	atoi := goRange(test, file, "Atoi")
	result, err := json.Marshal(map[string]any{
		"itemDefaults": map[string]any{"editRange": map[string]any{"insert": atoi, "replace": atoi}},
		"items":        []any{map[string]any{"label": "Atoi", "textEdit": map[string]any{"newText": "Atoi", "range": atoi}}},
	})
	if err != nil {
		test.Fatal(err)
	}
	mapped, ok := server.newMapper(file, false).mapJSON(result)
	if !ok {
		test.Fatal("completion list was dropped")
	}
	raw, err := json.Marshal(mapped)
	if err != nil {
		test.Fatal(err)
	}
	var decoded struct {
		ItemDefaults struct {
			EditRange struct {
				Insert lspRange `json:"insert"`
			} `json:"editRange"`
		} `json:"itemDefaults"`
		Items []struct {
			TextEdit struct {
				Range lspRange `json:"range"`
			} `json:"textEdit"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		test.Fatal(err)
	}
	want := golRange(test, "Atoi")
	if decoded.ItemDefaults.EditRange.Insert != want || len(decoded.Items) != 1 || decoded.Items[0].TextEdit.Range != want {
		test.Errorf("got %s, want ranges at %+v", raw, want)
	}
}

func TestMapperRewritesLocations(test *testing.T) {
	server, file := newMappingServer(test)
	result, err := json.Marshal([]any{
		map[string]any{"uri": pathToURI(file.goPath), "range": goRange(test, file, "count")},
		map[string]any{"uri": "file:///usr/lib/go/src/strconv/atoi.go", "range": lspRange{Start: position{Line: 3, Character: 1}}},
	})
	if err != nil {
		test.Fatal(err)
	}
	mapped, ok := server.newMapper(file, false).mapJSON(result)
	if !ok {
		test.Fatal("locations were dropped")
	}
	locations, ok := convertJSON[[]struct {
		URI   string   `json:"uri"`
		Range lspRange `json:"range"`
	}](mapped)
	if !ok || len(locations) != 2 {
		test.Fatalf("got %v", mapped)
	}
	if locations[0].URI != pathToURI("/work/p/x.gol") || locations[0].Range != golRange(test, "count") {
		test.Errorf("generated location = %+v, want x.gol at %+v", locations[0], golRange(test, "count"))
	}
	if locations[1].URI != "file:///usr/lib/go/src/strconv/atoi.go" || locations[1].Range.Start != (position{Line: 3, Character: 1}) {
		test.Errorf("plain Go location changed: %+v", locations[1])
	}
}

func TestStrictMapperDropsGeneratedRanges(test *testing.T) {
	server, file := newMappingServer(test)
	diagnostics, err := json.Marshal([]any{
		map[string]any{"message": "in generated code", "range": goRange(test, file, "goto")},
		map[string]any{"message": "on count", "range": goRange(test, file, "count")},
	})
	if err != nil {
		test.Fatal(err)
	}
	mapped, ok := server.newMapper(file, true).mapJSON(diagnostics)
	kept, isList := mapped.([]any)
	if !ok || !isList || len(kept) != 1 {
		test.Fatalf("got %v, want only the diagnostic on count", mapped)
	}
}

func TestMapperMapsPlainEditRange(test *testing.T) {
	server, file := newMappingServer(test)
	result, err := json.Marshal(map[string]any{"itemDefaults": map[string]any{"editRange": goRange(test, file, "Atoi")}})
	if err != nil {
		test.Fatal(err)
	}
	mapped, ok := server.newMapper(file, false).mapJSON(result)
	if !ok {
		test.Fatal("completion list was dropped")
	}
	decoded, ok := convertJSON[struct {
		ItemDefaults struct {
			EditRange lspRange `json:"editRange"`
		} `json:"itemDefaults"`
	}](mapped)
	if !ok || decoded.ItemDefaults.EditRange != golRange(test, "Atoi") {
		test.Errorf("editRange = %+v, want %+v", decoded.ItemDefaults.EditRange, golRange(test, "Atoi"))
	}
}
