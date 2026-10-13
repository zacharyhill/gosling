package lsp

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// rangeKeys are the LSP fields that hold a range in the document of the
// object that contains them. editRange can also hold an object with insert
// and replace ranges.
var rangeKeys = map[string]bool{
	"range":                true,
	"targetRange":          true,
	"targetSelectionRange": true,
	"selectionRange":       true,
	"insert":               true,
	"replace":              true,
	"editRange":            true,
}

func pathToURI(path string) string {
	slashPath := filepath.ToSlash(path)
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath // a Windows path such as C:/dir
	}
	return (&url.URL{Scheme: "file", Path: slashPath}).String()
}

func uriToPath(uri string) (string, bool) {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "file" {
		return "", false
	}
	path := parsed.Path
	if len(path) > 2 && path[0] == '/' && path[2] == ':' {
		path = path[1:] // a Windows drive letter, such as /C:/dir
	}
	return filepath.FromSlash(path), true
}

func decodeJSON(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	err := decoder.Decode(&value)
	return value, err
}

// convertJSON converts a decoded JSON value to a typed value.
func convertJSON[T any](value any) (T, bool) {
	var converted T
	raw, err := json.Marshal(value)
	if err != nil {
		return converted, false
	}
	return converted, json.Unmarshal(raw, &converted) == nil
}

// currentGolText returns the editor text of the .gol file of file, or the
// text that file came from.
func (server *server) currentGolText(file *generatedFile) []byte {
	if document, ok := server.documents[file.golPath]; ok {
		return document.text
	}
	return file.golText
}

// golToGo maps a position in the editor text of a .gol file to its
// generated file. Only positions in copied tokens map.
func (server *server) golToGo(file *generatedFile, golPosition position) (position, bool) {
	current := server.currentGolText(file)
	golOffset, ok := offsetAt(current, golPosition)
	if !ok {
		return position{}, false
	}
	if !bytes.Equal(current, file.golText) {
		if golOffset, ok = diffText(file.golText, current).toOld(golOffset); !ok {
			return position{}, false
		}
	}
	goOffset, ok := file.sourceMap.GoOffset(golOffset)
	if !ok {
		return position{}, false
	}
	return positionAt(file.goText, goOffset), true
}

// mapper maps the positions in a gopls message from generated files to
// .gol files.
type mapper struct {
	server *server
	// origin is the generated file that holds ranges without a URI, such
	// as the ranges in a hover result.
	origin *generatedFile
	// strict maps only positions in copied tokens. Otherwise a position in
	// generated code maps to the nearest token before it.
	strict bool
	shifts map[*generatedFile]textShift
}

func (server *server) newMapper(origin *generatedFile, strict bool) *mapper {
	return &mapper{server: server, origin: origin, strict: strict, shifts: map[*generatedFile]textShift{}}
}

func (mapper *mapper) mapJSON(raw json.RawMessage) (any, bool) {
	value, err := decodeJSON(raw)
	if err != nil {
		mapper.server.logger.Printf("decode gopls message: %v", err)
		return nil, false
	}
	return mapper.walk(value, mapper.origin)
}

// walk maps every range in value. A range that does not map removes the
// object that holds it. In an array, only that element goes.
func (mapper *mapper) walk(value any, current *generatedFile) (any, bool) {
	switch typed := value.(type) {
	case []any:
		kept := typed[:0]
		for _, element := range typed {
			if mapped, ok := mapper.walk(element, current); ok {
				kept = append(kept, mapped)
			}
		}
		return kept, true
	case map[string]any:
		return mapper.walkObject(typed, current)
	}
	return value, true
}

func (mapper *mapper) walkObject(object map[string]any, current *generatedFile) (any, bool) {
	current = mapper.switchDocument(object, current)
	for key, child := range object {
		var mapped any
		ok := true
		switch {
		case key == "originSelectionRange":
			mapped, ok = mapper.mapRange(child, mapper.origin)
		case rangeKeys[key] && isRange(child):
			if current == nil {
				continue
			}
			mapped, ok = mapper.mapRange(child, current)
		default:
			mapped, ok = mapper.walk(child, current)
		}
		if !ok {
			return nil, false
		}
		object[key] = mapped
	}
	return object, true
}

// switchDocument handles an object with a URI. Ranges in it belong to that
// document. A generated file's URI becomes the URI of its .gol file. It
// returns nil for a document that is not generated.
func (mapper *mapper) switchDocument(object map[string]any, current *generatedFile) *generatedFile {
	for _, key := range []string{"uri", "targetUri"} {
		uri, ok := object[key].(string)
		if !ok {
			continue
		}
		path, ok := uriToPath(uri)
		if !ok {
			return nil
		}
		file := mapper.server.generatedFor(path)
		if file != nil {
			object[key] = pathToURI(file.golPath)
		}
		return file
	}
	return current
}

func isRange(value any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	_, hasStart := object["start"]
	_, hasEnd := object["end"]
	return hasStart && hasEnd
}

func (mapper *mapper) mapRange(value any, file *generatedFile) (any, bool) {
	if file == nil {
		return value, true
	}
	original, ok := convertJSON[lspRange](value)
	if !ok {
		return nil, false
	}
	start, ok := mapper.goToGol(file, original.Start)
	if !ok {
		return nil, false
	}
	end, ok := mapper.goToGol(file, original.End)
	if !ok || end.Line < start.Line || (end.Line == start.Line && end.Character < start.Character) {
		end = start
	}
	return lspRange{Start: start, End: end}, true
}

// goToGol maps a position in a generated file to the editor text of its
// .gol file.
func (mapper *mapper) goToGol(file *generatedFile, goPosition position) (position, bool) {
	goOffset, ok := offsetAt(file.goText, goPosition)
	if !ok {
		return position{}, false
	}
	var golOffset int
	if mapper.strict {
		golOffset, ok = file.sourceMap.GolOffset(goOffset)
	} else {
		golOffset, ok = file.sourceMap.GolOffsetNear(goOffset)
	}
	if !ok {
		return position{}, false
	}
	current := mapper.server.currentGolText(file)
	if !bytes.Equal(current, file.golText) {
		shift, cached := mapper.shifts[file]
		if !cached {
			shift = diffText(file.golText, current)
			mapper.shifts[file] = shift
		}
		if golOffset, ok = shift.toNew(golOffset); !ok {
			return position{}, false
		}
	}
	return positionAt(current, golOffset), true
}

// generatedFor returns the generated file at path, or nil if gosling did
// not generate it. A file of a package without open documents is generated
// again from disk and matched against the file on disk.
func (server *server) generatedFor(path string) *generatedFile {
	if packageState, ok := server.packages[filepath.Dir(path)]; ok {
		return packageState.files[path]
	}
	if file, cached := server.external[path]; cached {
		return file
	}
	server.external[path] = nil
	if !strings.HasSuffix(path, ".go") {
		return nil
	}
	if _, err := os.Stat(golPathFor(path)); err != nil {
		return nil
	}
	server.loadExternal(filepath.Dir(path))
	return server.external[path]
}

// loadExternal generates a package from disk. Files whose output differs
// from the file on disk are out of date, so they get no mapping.
func (server *server) loadExternal(dir string) {
	outputs, err := server.generator.Generate(dir, nil, true)
	if err != nil {
		server.logger.Printf("generate %s: %v", dir, err)
		return
	}
	for _, output := range outputs {
		onDisk, err := os.ReadFile(output.Path)
		if err != nil || !bytes.Equal(onDisk, output.Src) {
			continue
		}
		golText, err := os.ReadFile(golPathFor(output.Path))
		if err != nil {
			continue
		}
		server.external[output.Path] = newGeneratedFile(output, golText)
	}
}
