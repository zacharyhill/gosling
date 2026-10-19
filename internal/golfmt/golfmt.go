// Package golfmt formats .gol files. gofmt cannot parse gosling syntax, so
// golfmt runs gofmt on the Go code that preprocessing makes. Preprocessing
// keeps lines and copies most tokens. golfmt then copies the space between
// each pair of copied tokens back to the .gol file. It changes only the
// space between tokens, never the tokens.
package golfmt

import (
	"bytes"
	"errors"
	"go/format"
	"go/scanner"
	"go/token"
	"strings"

	"github.com/zacharyhill/gosling/internal/preprocess"
)

// Edit replaces src[Start:End] with Text.
type Edit struct {
	Start, End int
	Text       string
}

var errChangedTokens = errors.New("internal error: formatting would change the tokens")

// Source returns src formatted.
func Source(src []byte) ([]byte, error) {
	edits, err := Edits(src)
	if err != nil {
		return nil, err
	}
	return applyEdits(src, edits), nil
}

// Edits returns the edits that format src, in order. A syntax error returns
// a *scanner.Error or a scanner.ErrorList with positions in src.
func Edits(src []byte) ([]Edit, error) {
	preprocessed, err := preprocess.Process(src)
	if err != nil {
		return nil, mapSourceError(src, nil, err)
	}
	formatted, err := format.Source(preprocessed.Src)
	if err != nil {
		return nil, mapSourceError(src, preprocessed, err)
	}
	layout := newLayout(src)
	layout.copyGaps(newGofmtResult(preprocessed, formatted))
	layout.cleanKeptGaps()
	layout.alignLetGroups(formatted)
	layout.alignComments(formatted)
	edits := layout.buildEdits()
	if !sameTokens(src, preprocessed, applyEdits(src, edits)) {
		return nil, errChangedTokens
	}
	return edits, nil
}

func applyEdits(src []byte, edits []Edit) []byte {
	var result []byte
	copiedUpTo := 0
	for _, edit := range edits {
		result = append(result, src[copiedUpTo:edit.Start]...)
		result = append(result, edit.Text...)
		copiedUpTo = edit.End
	}
	return append(result, src[copiedUpTo:]...)
}

// mapSourceError gives err positions in src. Errors from gofmt have positions
// in the preprocessed code.
func mapSourceError(src []byte, preprocessed *preprocess.Result, err error) error {
	var preprocessError *preprocess.Error
	if errors.As(err, &preprocessError) {
		return &scanner.Error{Pos: positionAt(src, preprocessError.Offset), Msg: preprocessError.Msg}
	}
	var parseErrors scanner.ErrorList
	if preprocessed == nil || !errors.As(err, &parseErrors) {
		return err
	}
	mapped := make(scanner.ErrorList, 0, len(parseErrors))
	for _, parseError := range parseErrors {
		offset := min(preprocessed.OrigOffset(parseError.Pos.Offset), len(src))
		mapped = append(mapped, &scanner.Error{Pos: positionAt(src, offset), Msg: parseError.Msg})
	}
	return mapped
}

func positionAt(src []byte, offset int) token.Position {
	lineStart := bytes.LastIndexByte(src[:offset], '\n') + 1
	return token.Position{Offset: offset, Line: 1 + bytes.Count(src[:offset], []byte("\n")), Column: offset - lineStart + 1}
}

// sourceToken is a token or comment. text is the exact source text.
type sourceToken struct {
	start, end int
	kind       token.Token
	text       string
}

// scanTokens returns the tokens and comments of src. With automatic, it
// includes the semicolons that the scanner adds at line ends.
func scanTokens(src []byte, automatic bool) []sourceToken {
	tokenFile := token.NewFileSet().AddFile("", -1, len(src))
	var sourceScanner scanner.Scanner
	// gosling syntax such as "?" is not Go, so the scanner reports errors.
	sourceScanner.Init(tokenFile, src, func(token.Position, string) {}, scanner.ScanComments)
	var tokens []sourceToken
	for {
		position, kind, literal := sourceScanner.Scan()
		if kind == token.EOF {
			return tokens
		}
		start := tokenFile.Offset(position)
		if kind == token.SEMICOLON && literal != ";" {
			if automatic {
				tokens = append(tokens, sourceToken{start: start, end: start, kind: kind, text: literal})
			}
			continue
		}
		end := findTokenEnd(src, start, kind, literal)
		tokens = append(tokens, sourceToken{start: start, end: end, kind: kind, text: string(src[start:end])})
	}
}

// findTokenEnd returns the end offset of a token. The scanner removes carriage
// returns from comments and raw strings, so their literals can be shorter
// than the source text.
func findTokenEnd(src []byte, start int, kind token.Token, literal string) int {
	switch {
	case kind == token.COMMENT && strings.HasPrefix(literal, "//"):
		if newline := bytes.IndexByte(src[start:], '\n'); newline >= 0 {
			return start + newline
		}
		return len(src)
	case kind == token.COMMENT:
		if closing := bytes.Index(src[start+len("/*"):], []byte("*/")); closing >= 0 {
			return start + len("/*") + closing + len("*/")
		}
		return len(src)
	case kind == token.STRING && strings.HasPrefix(literal, "`"):
		if closing := bytes.IndexByte(src[start+1:], '`'); closing >= 0 {
			return start + 1 + closing + 1
		}
		return len(src)
	case literal != "":
		return start + len(literal)
	}
	return start + len(kind.String())
}

// sameTokens reports whether formatted has the tokens of src, and whether
// both preprocess to the same tokens.
func sameTokens(src []byte, preprocessed *preprocess.Result, formatted []byte) bool {
	if !equalTokens(scanTokens(preprocess.MaskRegexes(src), true), scanTokens(preprocess.MaskRegexes(formatted), true)) {
		return false
	}
	reprocessed, err := preprocess.Process(formatted)
	if err != nil {
		return false
	}
	return equalTokens(scanTokens(preprocessed.Src, true), scanTokens(reprocessed.Src, true))
}

func equalTokens(left, right []sourceToken) bool {
	if len(left) != len(right) {
		return false
	}
	for tokenIndex := range left {
		if left[tokenIndex].kind != right[tokenIndex].kind || left[tokenIndex].text != right[tokenIndex].text {
			return false
		}
	}
	return true
}
