package lsp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

// mapRename maps a gopls rename result to the .gol files. A partial rename
// breaks the code, so one edit that does not map fails the rename.
func (server *server) mapRename(raw json.RawMessage) (workspaceEdit, error) {
	var received workspaceEdit
	if err := json.Unmarshal(raw, &received); err != nil {
		return workspaceEdit{}, err
	}
	var mapped workspaceEdit
	for uri, edits := range received.Changes {
		golURI, golEdits, err := server.mapFileEdits(uri, edits)
		if err != nil {
			return workspaceEdit{}, err
		}
		if mapped.Changes == nil {
			mapped.Changes = map[string][]textEdit{}
		}
		mapped.Changes[golURI] = golEdits
	}
	for _, change := range received.DocumentChanges {
		if change.Kind != "" {
			return workspaceEdit{}, errRenameFileChange
		}
		golURI, golEdits, err := server.mapFileEdits(change.TextDocument.URI, change.Edits)
		if err != nil {
			return workspaceEdit{}, err
		}
		if golURI != change.TextDocument.URI {
			change.TextDocument = versionedDocument{URI: golURI}
		}
		change.Edits = golEdits
		mapped.DocumentChanges = append(mapped.DocumentChanges, change)
	}
	return mapped, nil
}

// mapFileEdits maps the edits of one file. Edits in a file that gosling does
// not generate stay the same. gosling can copy one token to more than one
// place, so equal edits merge.
func (server *server) mapFileEdits(uri string, edits []textEdit) (string, []textEdit, error) {
	path, ok := uriToPath(uri)
	if !ok {
		return uri, edits, nil
	}
	file := server.generatedFor(path)
	if file == nil {
		if isGeneratedPath(path) {
			return "", nil, fmt.Errorf("cannot rename: %s is out of date, run gosling", path)
		}
		return uri, edits, nil
	}
	mapper := server.newMapper(file, true)
	seen := map[textEdit]bool{}
	var golEdits []textEdit
	for _, edit := range edits {
		golRange, ok := mapper.copiedRange(file, edit.Range)
		if !ok {
			return "", nil, errRenameGenerated
		}
		golEdit := textEdit{Range: golRange, NewText: edit.NewText}
		if !seen[golEdit] {
			seen[golEdit] = true
			golEdits = append(golEdits, golEdit)
		}
	}
	return pathToURI(file.golPath), golEdits, nil
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
