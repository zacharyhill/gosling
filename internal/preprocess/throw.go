package preprocess

import "go/token"

// ThrowMarker wraps the value of a throw statement. throw err becomes
// panic(__goslingThrow(err)). The panic makes the type checker treat the
// statement as terminating, as the lowered goto or return is.
const ThrowMarker = "__goslingThrow"

// isThrow reports whether the token at tokenIndex is a throw keyword. A
// throw starts a statement and its value starts on the same line. So
// throw(x), throw = x, and throw.f stay identifiers.
func (processor *processor) isThrow(tokenIndex int) bool {
	if !processor.tokens[tokenIndex].isIdent("throw") || !processor.atStmtStart(tokenIndex) {
		return false
	}
	switch processor.tokenAt(tokenIndex + 1).kind {
	case token.IDENT, token.STRING, token.CHAR, token.INT, token.FUNC, token.AND, token.MUL, token.SEMICOLON, token.RBRACE, token.EOF:
		return true
	case token.LPAREN:
		return !processor.touchesNext(tokenIndex)
	}
	return false
}

// rewriteThrow rewrites the throw statement at token tokenIndex.
func (processor *processor) rewriteThrow(tokenIndex int) error {
	keyword, value := processor.tokens[tokenIndex], processor.tokenAt(tokenIndex+1)
	switch value.kind {
	case token.SEMICOLON, token.RBRACE, token.EOF:
		return &Error{keyword.offset, "throw needs an error value"}
	}
	processor.replace(keyword.offset, value.offset, "panic("+ThrowMarker+"(")
	processor.pendingClosers = append(processor.pendingClosers, pendingCloser{start: processor.nesting(), text: "))"})
	return nil
}
