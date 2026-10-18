package lsp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// workspaceEdit holds the parts of an LSP WorkspaceEdit that a rename uses.
type workspaceEdit struct {
	Changes         map[string][]textEdit `json:"changes,omitempty"`
	DocumentChanges []textDocumentEdit    `json:"documentChanges,omitempty"`
}

// textDocumentEdit is one entry of documentChanges. A file operation, such
// as a file rename, has a kind.
type textDocumentEdit struct {
	Kind         string            `json:"kind,omitempty"`
	TextDocument versionedDocument `json:"textDocument"`
	Edits        []textEdit        `json:"edits"`
}

// versionedDocument has a nil version for a .gol file, because the server
// does not track editor versions.
type versionedDocument struct {
	URI     string `json:"uri"`
	Version *int   `json:"version"`
}

type textEdit struct {
	Range   lspRange `json:"range"`
	NewText string   `json:"newText"`
}

var (
	errRenameGenerated  = errors.New("cannot rename: the name also occurs in code that gosling generates")
	errRenameFileChange = errors.New("cannot rename: the rename creates, renames, or deletes files")
)

// fileRename holds the edits of a rename in one file.
type fileRename struct {
	// uri is the file to edit. It is the .gol file of a generated file.
	uri   string
	edits []textEdit
	// file is the generated file, or nil for a file that gosling does not
	// generate.
	file *generatedFile
	// goEdits holds the gopls edits of a generated file, and generatedEdits
	// the ones in text that gosling generates.
	goEdits        []textEdit
	generatedEdits []textEdit
}

func (rename *fileRename) addEdit(edit textEdit) {
	// gosling can copy one token to more than one place, so equal edits merge.
	if !slices.Contains(rename.edits, edit) {
		rename.edits = append(rename.edits, edit)
	}
}

// mapRename maps a gopls rename result to the .gol files. A partial rename
// breaks the code, so the rename fails if one edit does not map.
func (server *server) mapRename(raw json.RawMessage) (workspaceEdit, error) {
	var received workspaceEdit
	if err := json.Unmarshal(raw, &received); err != nil {
		return workspaceEdit{}, err
	}
	var changes, documentChanges []*fileRename
	for uri, edits := range received.Changes {
		rename, err := server.mapFileEdits(uri, edits)
		if err != nil {
			return workspaceEdit{}, err
		}
		changes = append(changes, rename)
	}
	for _, change := range received.DocumentChanges {
		if change.Kind != "" {
			return workspaceEdit{}, errRenameFileChange
		}
		rename, err := server.mapFileEdits(change.TextDocument.URI, change.Edits)
		if err != nil {
			return workspaceEdit{}, err
		}
		documentChanges = append(documentChanges, rename)
	}
	if err := server.mapGeneratedEdits(append(slices.Clone(changes), documentChanges...)); err != nil {
		return workspaceEdit{}, err
	}
	return buildMappedEdit(received, changes, documentChanges), nil
}

// buildMappedEdit builds the .gol workspace edit from the mapped renames of
// received.Changes and received.DocumentChanges.
func buildMappedEdit(received workspaceEdit, changes, documentChanges []*fileRename) workspaceEdit {
	var mapped workspaceEdit
	for _, rename := range changes {
		if mapped.Changes == nil {
			mapped.Changes = map[string][]textEdit{}
		}
		mapped.Changes[rename.uri] = rename.edits
	}
	for changeIndex, change := range received.DocumentChanges {
		rename := documentChanges[changeIndex]
		if rename.uri != change.TextDocument.URI {
			change.TextDocument = versionedDocument{URI: rename.uri}
		}
		change.Edits = rename.edits
		mapped.DocumentChanges = append(mapped.DocumentChanges, change)
	}
	return mapped
}

// mapFileEdits maps the edits of copied tokens in one file. It keeps the
// edits in generated text for mapGeneratedEdits. Edits in a file that
// gosling does not generate stay the same.
func (server *server) mapFileEdits(uri string, edits []textEdit) (*fileRename, error) {
	path, ok := uriToPath(uri)
	if !ok {
		return &fileRename{uri: uri, edits: edits}, nil
	}
	file := server.generatedFor(path)
	if file == nil {
		if isGeneratedPath(path) {
			return nil, fmt.Errorf("cannot rename: %s is out of date, run gosling", path)
		}
		return &fileRename{uri: uri, edits: edits}, nil
	}
	mapper := server.newMapper(file, true)
	rename := &fileRename{uri: pathToURI(file.golPath), file: file, goEdits: edits}
	for _, edit := range edits {
		golRange, ok := mapper.copiedRange(file, edit.Range)
		if !ok {
			rename.generatedEdits = append(rename.generatedEdits, edit)
			continue
		}
		rename.addEdit(textEdit{Range: golRange, NewText: edit.NewText})
	}
	return rename, nil
}

// copiedRange maps a range of a generated file to the editor text of its .gol
// file. The .gol file must hold the same text, so an edit there has the same
// effect.
func (mapper *mapper) copiedRange(file *generatedFile, goRange lspRange) (lspRange, bool) {
	goStart, startOK := offsetAt(file.goText, goRange.Start)
	goEnd, endOK := offsetAt(file.goText, goRange.End)
	if !startOK || !endOK || goEnd < goStart {
		return lspRange{}, false
	}
	golStart, ok := file.sourceMap.GolOffset(goStart)
	golEnd := golStart + goEnd - goStart
	if !ok || golEnd > len(file.golText) || !bytes.Equal(file.golText[golStart:golEnd], file.goText[goStart:goEnd]) {
		return lspRange{}, false
	}
	start, startOK := mapper.currentPosition(file, golStart)
	end, endOK := mapper.currentPosition(file, golEnd)
	if !startOK || !endOK {
		return lspRange{}, false
	}
	return lspRange{Start: start, End: end}, true
}

// mapPrepareRename maps a prepareRename result. It returns nil if the name
// is in generated code, so the editor reports that the name cannot change.
func (server *server) mapPrepareRename(raw json.RawMessage, origin *generatedFile) any {
	result, err := decodeJSON(raw)
	if err != nil || origin == nil {
		return nil
	}
	mapper := server.newMapper(origin, true)
	if isRange(result) {
		goRange, ok := convertJSON[lspRange](result)
		if !ok {
			return nil
		}
		golRange, ok := mapper.copiedRange(origin, goRange)
		if !ok {
			return nil
		}
		return golRange
	}
	mapped, ok := mapper.walk(result, origin)
	if !ok {
		return nil
	}
	return mapped
}
