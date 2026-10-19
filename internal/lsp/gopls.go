package lsp

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
)

func (server *server) handleGoplsMessage(message *rpcMessage) error {
	switch {
	case message.isResponse():
		return server.handleGoplsResponse(message)
	case message.Method == "client/registerCapability":
		return server.forwardRegistrations(message)
	case message.Method == "textDocument/publishDiagnostics":
		return server.handleGoplsDiagnostics(message)
	case message.Method == "window/showMessage" && server.isGeneratedFileWarning(message):
		return nil
	}
	return server.writeClient(message)
}

// generatedFileWarningRE matches the gopls warning for the first edit of a
// generated file. gopls names only the base name of the file.
var generatedFileWarningRE = regexp.MustCompile(`^Warning: editing (.+\.go), a generated file\.$`)

// isGeneratedFileWarning reports whether message is the gopls warning about
// an edit of a file that the server generates. The server makes these
// edits, not the user.
func (server *server) isGeneratedFileWarning(message *rpcMessage) bool {
	var params struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(message.Params, &params) != nil {
		return false
	}
	matches := generatedFileWarningRE.FindStringSubmatch(params.Message)
	if matches == nil {
		return false
	}
	warnedFileName := matches[1]
	for _, packageState := range server.packages {
		for goPath := range packageState.files {
			if filepath.Base(goPath) == warnedFileName {
				return true
			}
		}
	}
	return false
}

func (server *server) handleGoplsResponse(message *rpcMessage) error {
	request, ok := server.pending[string(message.ID)]
	if !ok {
		server.logger.Printf("gopls response to unknown request %s", message.ID)
		return nil
	}
	delete(server.pending, string(message.ID))
	if message.Error != nil || message.Result == nil {
		return server.writeClient(message)
	}
	switch request.method {
	case "initialize":
		return server.respondClient(message.ID, map[string]any{
			"capabilities": serverCapabilities,
			"serverInfo":   map[string]any{"name": "gosling"},
		})
	case "textDocument/rename":
		edit, err := server.mapRename(message.Result)
		if err != nil {
			return server.respondError(message.ID, requestFailed, err)
		}
		return server.respondClient(message.ID, edit)
	case "textDocument/prepareRename":
		if request.nameRange != nil && string(message.Result) != "null" {
			return server.respondClient(message.ID, request.nameRange)
		}
		return server.respondClient(message.ID, server.mapPrepareRename(message.Result, request.origin))
	}
	result, err := decodeJSON(message.Result)
	if err != nil {
		return server.respondError(message.ID, invalidRequest, err)
	}
	if hover, ok := result.(map[string]any); ok && request.method == "textDocument/hover" {
		// The hover range is optional. Without it, a hover on a name that
		// gosling changed still shows.
		delete(hover, "range")
	}
	mapped, ok := server.newMapper(request.origin, false).walk(result, request.origin)
	if !ok {
		mapped = nil
	}
	return server.respondClient(message.ID, mapped)
}

// forwardRegistrations passes on only workspace registrations, such as file
// watchers. A document registration from gopls would make the editor send
// .go requests to this server.
func (server *server) forwardRegistrations(message *rpcMessage) error {
	var params struct {
		Registrations []json.RawMessage `json:"registrations"`
	}
	if err := json.Unmarshal(message.Params, &params); err != nil {
		return server.writeGopls(&rpcMessage{ID: message.ID})
	}
	var kept []json.RawMessage
	for _, registration := range params.Registrations {
		var header struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(registration, &header) == nil && strings.HasPrefix(header.Method, "workspace/") {
			kept = append(kept, registration)
		}
	}
	if len(kept) == 0 {
		return server.writeGopls(&rpcMessage{ID: message.ID})
	}
	raw, err := json.Marshal(map[string]any{"registrations": kept})
	if err != nil {
		return err
	}
	message.Params = raw
	return server.writeClient(message)
}

// handleGoplsDiagnostics keeps the diagnostics of generated files and shows
// them on the .gol documents. It drops diagnostics for other files.
func (server *server) handleGoplsDiagnostics(message *rpcMessage) error {
	var params struct {
		URI         string            `json:"uri"`
		Diagnostics []json.RawMessage `json:"diagnostics"`
	}
	if err := json.Unmarshal(message.Params, &params); err != nil {
		server.logger.Printf("bad diagnostics from gopls: %v", err)
		return nil
	}
	goPath, ok := uriToPath(params.URI)
	if !ok {
		return nil
	}
	packageState, ok := server.packages[filepath.Dir(goPath)]
	if !ok {
		return nil
	}
	file, ok := packageState.files[goPath]
	if !ok {
		return nil
	}
	packageState.goplsDiagnostics[goPath] = params.Diagnostics
	if _, open := server.documents[file.golPath]; !open {
		return nil
	}
	return server.publishDiagnostics(file.golPath, server.documentDiagnostics(packageState, file.golPath))
}
