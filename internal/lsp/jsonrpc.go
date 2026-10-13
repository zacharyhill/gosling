// Package lsp is a language server for .gol files. It runs gopls on the
// generated .go files and maps positions between the two.
package lsp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"strconv"
)

// rpcMessage is a JSON-RPC 2.0 request, notification, or response. The proxy
// keeps params and results as raw JSON and decodes only what it changes.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

func (message *rpcMessage) isRequest() bool      { return message.Method != "" && message.ID != nil }
func (message *rpcMessage) isNotification() bool { return message.Method != "" && message.ID == nil }
func (message *rpcMessage) isResponse() bool     { return message.Method == "" && message.ID != nil }

func readMessage(reader *bufio.Reader) (*rpcMessage, error) {
	header, err := textproto.NewReader(reader).ReadMIMEHeader()
	if err != nil {
		return nil, err
	}
	lengthText := header.Get("Content-Length")
	if lengthText == "" {
		return nil, errors.New("message header has no Content-Length")
	}
	length, err := strconv.Atoi(lengthText)
	if err != nil {
		return nil, fmt.Errorf("bad Content-Length %q: %w", lengthText, err)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	var message rpcMessage
	if err := json.Unmarshal(body, &message); err != nil {
		return nil, fmt.Errorf("bad message body: %w", err)
	}
	return &message, nil
}

func writeMessage(writer io.Writer, message *rpcMessage) error {
	message.JSONRPC = "2.0"
	// JSON-RPC requires a result or an error in every response.
	if message.isResponse() && message.Result == nil && message.Error == nil {
		message.Result = json.RawMessage("null")
	}
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = writer.Write(body)
	return err
}
