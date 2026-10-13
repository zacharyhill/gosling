package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestMessageRoundTrip(test *testing.T) {
	var buffer bytes.Buffer
	sent := &rpcMessage{ID: json.RawMessage(`7`), Method: "textDocument/hover", Params: json.RawMessage(`{"x":1}`)}
	if err := writeMessage(&buffer, sent); err != nil {
		test.Fatal(err)
	}
	if !strings.HasPrefix(buffer.String(), "Content-Length: ") {
		test.Fatalf("missing header: %q", buffer.String())
	}
	received, err := readMessage(bufio.NewReader(&buffer))
	if err != nil {
		test.Fatal(err)
	}
	if string(received.ID) != "7" || received.Method != "textDocument/hover" || string(received.Params) != `{"x":1}` {
		test.Errorf("got %+v", received)
	}
	if !received.isRequest() || received.isNotification() || received.isResponse() {
		test.Errorf("kind checks wrong for %+v", received)
	}
}

func TestResponseKeepsNullResult(test *testing.T) {
	var buffer bytes.Buffer
	if err := writeMessage(&buffer, &rpcMessage{ID: json.RawMessage(`"a"`)}); err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(buffer.String(), `"result":null`) {
		test.Errorf("response without result must send null: %q", buffer.String())
	}
	received, err := readMessage(bufio.NewReader(&buffer))
	if err != nil {
		test.Fatal(err)
	}
	if !received.isResponse() {
		test.Errorf("got %+v, want a response", received)
	}
}

func TestReadMessageRejectsMissingLength(test *testing.T) {
	_, err := readMessage(bufio.NewReader(strings.NewReader("Content-Type: x\r\n\r\n{}")))
	if err == nil {
		test.Fatal("want an error for a header without Content-Length")
	}
}
