package lsp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// messageTypeWarning is the LSP MessageType for a warning in window/showMessage.
const messageTypeWarning = 2

func buildShowMessage(test *testing.T, text string) *rpcMessage {
	test.Helper()
	params, err := json.Marshal(map[string]any{"type": messageTypeWarning, "message": text})
	if err != nil {
		test.Fatal(err)
	}
	return &rpcMessage{Method: "window/showMessage", Params: params}
}

// TestGeneratedFileWarningIsDropped checks the gopls warning about an edit
// of a generated file. The server makes these edits, so the warning goes
// only to the client for other files.
func TestGeneratedFileWarningIsDropped(test *testing.T) {
	server, _ := newMappingServer(test)
	var client bytes.Buffer
	server.client = &client
	if err := server.handleGoplsMessage(buildShowMessage(test, "Warning: editing x.go, a generated file.")); err != nil {
		test.Fatal(err)
	}
	if client.Len() != 0 {
		test.Errorf("the client got %q, want nothing", client.String())
	}
	for _, text := range []string{"Warning: editing other.go, a generated file.", "gopls: some other message"} {
		if err := server.handleGoplsMessage(buildShowMessage(test, text)); err != nil {
			test.Fatal(err)
		}
		if !strings.Contains(client.String(), text) {
			test.Errorf("the client did not get %q", text)
		}
		client.Reset()
	}
}
