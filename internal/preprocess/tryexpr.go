package preprocess

import "go/token"

// TryMarker replaces a try expression. try f() catch d becomes
// __goslingTry(f())(d).
const TryMarker = "__goslingTry"

// rewriteTryExpression rewrites a try expression that starts at token
// tokenIndex. It changes nothing if the try has no catch at the same depth.
// Then try is an identifier.
func (processor *processor) rewriteTryExpression(tokenIndex int) error {
	keyword, call := processor.tokens[tokenIndex], processor.tokenAt(tokenIndex+1)
	switch call.kind {
	case token.IDENT, token.LPAREN, token.FUNC, token.MUL:
	default:
		return nil
	}
	catchIndex, ok := processor.findExpressionCatch(tokenIndex + 1)
	if !ok {
		return nil
	}
	if processor.tokenAt(catchIndex + 1).is(token.LPAREN) {
		return &Error{processor.tokens[catchIndex].offset, "a try expression cannot use the error; use a try statement"}
	}
	processor.replace(keyword.offset, call.offset, TryMarker+"(")
	processor.scheduled[catchIndex] = ")("
	// The fallback ends where the try expression ends.
	processor.pendingClosers = append(processor.pendingClosers, pendingCloser{start: processor.nesting(), text: ")"})
	return nil
}

// findExpressionCatch returns the index of the catch keyword at bracket
// depth zero after token from. The search stops at the end of the
// expression.
func (processor *processor) findExpressionCatch(from int) (int, bool) {
	depth := 0
	for tokenIndex := from; tokenIndex < len(processor.tokens); tokenIndex++ {
		current := processor.tokens[tokenIndex]
		switch current.kind {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			if depth == 0 {
				return 0, false
			}
			depth--
		case token.SEMICOLON, token.COMMA, token.COLON:
			if depth == 0 {
				return 0, false
			}
		case token.IDENT:
			if depth == 0 && current.text == "catch" {
				return tokenIndex, true
			}
		}
	}
	return 0, false
}
