package lsp

import (
	"encoding/json"
	"path/filepath"
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
	}
	return server.writeClient(message)
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
	if request.method == "initialize" {
		return server.respondClient(message.ID, map[string]any{
			"capabilities": serverCapabilities,
			"serverInfo":   map[string]any{"name": "gosling"},
		})
	}
	result, err := decodeJSON(message.Result)
	if err != nil {
		return server.respondError(message.ID, err)
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
