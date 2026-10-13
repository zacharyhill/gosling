package lsp

import (
	"encoding/json"
	"path/filepath"
)

// positionMethods are the requests on .gol documents that the server
// forwards to gopls with a mapped position.
var positionMethods = map[string]bool{
	"textDocument/definition":        true,
	"textDocument/typeDefinition":    true,
	"textDocument/implementation":    true,
	"textDocument/references":        true,
	"textDocument/hover":             true,
	"textDocument/documentHighlight": true,
	"textDocument/completion":        true,
	"textDocument/signatureHelp":     true,
	"textDocument/documentSymbol":    true,
	"textDocument/rename":            true,
	"textDocument/prepareRename":     true,
}

// serverCapabilities lists only the features that work on .gol files.
// Formatting would change generated code, so it is left out.
var serverCapabilities = map[string]any{
	"textDocumentSync": map[string]any{
		"openClose": true,
		"change":    1, // full text on each change
		"save":      map[string]any{"includeText": false},
	},
	"definitionProvider":        true,
	"typeDefinitionProvider":    true,
	"implementationProvider":    true,
	"referencesProvider":        true,
	"hoverProvider":             true,
	"documentHighlightProvider": true,
	"documentSymbolProvider":    true,
	"completionProvider":        map[string]any{"triggerCharacters": []string{"."}},
	"signatureHelpProvider":     map[string]any{"triggerCharacters": []string{"(", ","}},
	"renameProvider":            map[string]any{"prepareProvider": true},
}

func (server *server) handleClientMessage(message *rpcMessage) error {
	switch {
	case message.isResponse():
		return server.writeGopls(message)
	case message.isNotification():
		return server.handleClientNotification(message)
	}
	return server.handleClientRequest(message)
}

type textDocumentParams struct {
	TextDocument struct {
		URI  string `json:"uri"`
		Text string `json:"text"`
	} `json:"textDocument"`
	ContentChanges []struct {
		Range *lspRange `json:"range"`
		Text  string    `json:"text"`
	} `json:"contentChanges"`
}

func (server *server) handleClientNotification(message *rpcMessage) error {
	if message.Method == "exit" {
		server.exiting = true
		return server.writeGopls(message)
	}
	var params textDocumentParams
	golPath, isGol := "", false
	if json.Unmarshal(message.Params, &params) == nil {
		golPath, isGol = golPathOf(params.TextDocument.URI)
	}
	if !isGol {
		if message.Method == "workspace/didChangeWatchedFiles" {
			clear(server.external)
		}
		return server.writeGopls(message)
	}
	switch message.Method {
	case "textDocument/didOpen":
		return server.openDocument(golPath, []byte(params.TextDocument.Text))
	case "textDocument/didChange":
		server.changeDocument(golPath, params)
	case "textDocument/didClose":
		return server.closeDocument(golPath)
	case "textDocument/didSave":
		clear(server.external)
	}
	return nil
}

func (server *server) openDocument(golPath string, text []byte) error {
	server.documents[golPath] = &editorDocument{path: golPath, text: text}
	dir := filepath.Dir(golPath)
	packageState, ok := server.packages[dir]
	if !ok {
		packageState = newPackageState(dir)
		server.packages[dir] = packageState
	}
	return server.regenerate(packageState)
}

func (server *server) changeDocument(golPath string, params textDocumentParams) {
	document, ok := server.documents[golPath]
	if !ok {
		return
	}
	for _, change := range params.ContentChanges {
		document.text = applyChange(document.text, change.Range, change.Text)
	}
	if packageState, ok := server.packages[filepath.Dir(golPath)]; ok {
		packageState.dirty = true
		server.scheduleRegenerate()
	}
}

// applyChange applies one content change. A change without a range
// replaces the whole text.
func applyChange(text []byte, changeRange *lspRange, newText string) []byte {
	if changeRange == nil {
		return []byte(newText)
	}
	start, startOK := offsetAt(text, changeRange.Start)
	end, endOK := offsetAt(text, changeRange.End)
	if !startOK || !endOK || end < start {
		return text
	}
	changed := make([]byte, 0, len(text)-(end-start)+len(newText))
	changed = append(changed, text[:start]...)
	changed = append(changed, newText...)
	return append(changed, text[end:]...)
}

func (server *server) closeDocument(golPath string) error {
	delete(server.documents, golPath)
	packageState, ok := server.packages[filepath.Dir(golPath)]
	if !ok {
		return nil
	}
	if len(server.documentsIn(packageState.dir)) == 0 {
		return server.closePackage(packageState)
	}
	packageState.dirty = true
	server.scheduleRegenerate()
	return server.publishDiagnostics(golPath, nil)
}

func (server *server) handleClientRequest(message *rpcMessage) error {
	switch {
	case message.Method == "initialize":
		return server.forwardInitialize(message)
	case positionMethods[message.Method]:
		return server.forwardPositionRequest(message)
	}
	var params textDocumentParams
	if json.Unmarshal(message.Params, &params) == nil {
		if _, isGol := golPathOf(params.TextDocument.URI); isGol {
			return server.respondClient(message.ID, nil)
		}
	}
	server.pending[string(message.ID)] = pendingRequest{method: message.Method}
	return server.writeGopls(message)
}

// forwardInitialize removes the client's position encodings, so both sides
// use UTF-16, the LSP default.
func (server *server) forwardInitialize(message *rpcMessage) error {
	params, err := decodeJSON(message.Params)
	if err != nil {
		return server.respondError(message.ID, invalidRequest, err)
	}
	if paramsObject, ok := params.(map[string]any); ok {
		if capabilities, ok := paramsObject["capabilities"].(map[string]any); ok {
			if general, ok := capabilities["general"].(map[string]any); ok {
				delete(general, "positionEncodings")
			}
		}
	}
	message.Params, err = json.Marshal(params)
	if err != nil {
		return err
	}
	server.pending[string(message.ID)] = pendingRequest{method: message.Method}
	return server.writeGopls(message)
}

// JSON-RPC error codes. invalidRequest is for a request that the server
// cannot read. requestFailed is the LSP code for a valid request that fails.
const (
	invalidRequest = -32600
	requestFailed  = -32803
)

func (server *server) respondError(id json.RawMessage, code int, cause error) error {
	raw, err := json.Marshal(map[string]any{"code": code, "message": cause.Error()})
	if err != nil {
		return err
	}
	return server.writeClient(&rpcMessage{ID: id, Error: raw})
}

// forwardPositionRequest sends a request on a .gol document to gopls, with
// the URI and position of the generated file. A position in text that
// gosling generates gets an empty result.
func (server *server) forwardPositionRequest(message *rpcMessage) error {
	params, err := decodeJSON(message.Params)
	if err != nil {
		return server.respondError(message.ID, invalidRequest, err)
	}
	paramsObject, ok := params.(map[string]any)
	if !ok {
		return server.respondClient(message.ID, nil)
	}
	textDocument, _ := paramsObject["textDocument"].(map[string]any)
	uri, _ := textDocument["uri"].(string)
	golPath, isGol := golPathOf(uri)
	if !isGol {
		server.pending[string(message.ID)] = pendingRequest{method: message.Method}
		return server.writeGopls(message)
	}
	file, err := server.generatedForDocument(golPath)
	if err != nil || file == nil {
		return server.respondClient(message.ID, nil)
	}
	if !server.mapParamsPosition(paramsObject, file) {
		return server.respondClient(message.ID, nil)
	}
	textDocument["uri"] = pathToURI(file.goPath)
	if message.Params, err = json.Marshal(paramsObject); err != nil {
		return err
	}
	server.pending[string(message.ID)] = pendingRequest{method: message.Method, origin: file}
	return server.writeGopls(message)
}

func (server *server) generatedForDocument(golPath string) (*generatedFile, error) {
	packageState, err := server.packageFor(golPath)
	if err != nil || packageState == nil {
		return nil, err
	}
	return packageState.fileForGol(golPath), nil
}

// mapParamsPosition changes the position in params, if any, to the
// generated file.
func (server *server) mapParamsPosition(params map[string]any, file *generatedFile) bool {
	rawPosition, hasPosition := params["position"]
	if !hasPosition {
		return true
	}
	golPosition, ok := convertJSON[position](rawPosition)
	if !ok {
		return false
	}
	goPosition, ok := server.golToGo(file, golPosition)
	if !ok {
		return false
	}
	params["position"] = goPosition
	return true
}
