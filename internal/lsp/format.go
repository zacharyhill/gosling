package lsp

import (
	"encoding/json"
	"errors"
	"go/scanner"

	"github.com/zacharyhill/gosling/internal/golfmt"
)

// formatDocument answers a formatting request with the edits that format the
// editor text. A file with a syntax error gets no edits, because its
// diagnostics already show the error.
func (server *server) formatDocument(id json.RawMessage, golPath string) error {
	text := server.golText(golPath)
	edits, err := golfmt.Edits(text)
	var syntaxError *scanner.Error
	var syntaxErrors scanner.ErrorList
	if errors.As(err, &syntaxError) || errors.As(err, &syntaxErrors) {
		return server.respondClient(id, []textEdit{})
	}
	if err != nil {
		return server.respondError(id, requestFailed, err)
	}
	textEdits := make([]textEdit, 0, len(edits))
	for _, edit := range edits {
		editRange := lspRange{Start: positionAt(text, edit.Start), End: positionAt(text, edit.End)}
		textEdits = append(textEdits, textEdit{Range: editRange, NewText: edit.Text})
	}
	return server.respondClient(id, textEdits)
}
