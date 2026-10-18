package preprocess

import (
	"go/token"
	"strings"
)

// assertion rewrites `x as T` at the "as" at token tokenIndex into `x.(T)`.
// It returns the index of the last token of T.
func (processor *processor) assertion(tokenIndex int) (int, error) {
	keyword := processor.tokens[tokenIndex]
	if processor.isAfterOperator(processor.findOperandStart(tokenIndex - 1)) {
		return 0, &Error{keyword.offset, "as applies only to the operand before it; add parentheses, as in (a + b) as T"}
	}
	typeStart := processor.tokens[tokenIndex+1]
	typeEndIndex := processor.findTypeEnd(tokenIndex + 1)
	typeEnd := processor.tokenAt(typeEndIndex)
	// The type text stays in place, so the source map covers it.
	processor.replace(max(processor.tokens[tokenIndex-1].end(), processor.copiedUpTo), typeStart.offset, ".(")
	processor.replace(typeEnd.end(), typeEnd.end(), ")")
	return typeEndIndex, nil
}

// isAssertion reports whether the "as" at token tokenIndex is between an
// operand and a type. Elsewhere, as is a name.
func (processor *processor) isAssertion(tokenIndex int) bool {
	if tokenIndex == 0 || !processor.tokens[tokenIndex].isIdent("as") {
		return false
	}
	return endsOperand(processor.tokens[tokenIndex-1]) && startsType(processor.tokenAt(tokenIndex+1))
}

// findOperandStart returns the index of the first token of the operand that
// ends at token lastIndex, such as a.b(c)[d].
func (processor *processor) findOperandStart(lastIndex int) int {
	tokenIndex := lastIndex
	for tokenIndex > 0 {
		current := processor.tokens[tokenIndex]
		isClosing := current.is(token.RPAREN) || current.is(token.RBRACK) || current.is(token.RBRACE)
		if isClosing {
			tokenIndex = processor.matchOpen(tokenIndex)
		}
		if tokenIndex == 0 {
			return 0
		}
		previous := processor.tokenAt(tokenIndex - 1)
		switch {
		case previous.is(token.PERIOD) && tokenIndex >= 2:
			tokenIndex -= 2
		case isClosing && endsOperand(previous):
			tokenIndex-- // a call, an index, or a composite literal
		default:
			return tokenIndex
		}
	}
	return 0
}

// matchOpen returns the index of the bracket that opens the bracket at
// token closeIndex.
func (processor *processor) matchOpen(closeIndex int) int {
	depth := 0
	for tokenIndex := closeIndex; tokenIndex >= 0; tokenIndex-- {
		switch processor.tokens[tokenIndex].kind {
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth++
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth--
			if depth == 0 {
				return tokenIndex
			}
		}
	}
	return 0
}

// findTypeEnd returns the index of the last token of the type that starts at
// token tokenIndex.
func (processor *processor) findTypeEnd(tokenIndex int) int {
	switch current := processor.tokenAt(tokenIndex); current.kind {
	case token.MUL:
		return processor.findTypeEnd(tokenIndex + 1)
	case token.ARROW, token.CHAN:
		for processor.tokenAt(tokenIndex+1).is(token.CHAN) || processor.tokenAt(tokenIndex+1).is(token.ARROW) {
			tokenIndex++
		}
		return processor.findTypeEnd(tokenIndex + 1)
	case token.LBRACK, token.MAP:
		if current.is(token.MAP) {
			tokenIndex++
		}
		return processor.findTypeEnd(processor.findCloseOrEnd(tokenIndex) + 1)
	case token.INTERFACE, token.STRUCT:
		return processor.findCloseOrEnd(tokenIndex + 1)
	case token.FUNC:
		paramsEnd := processor.findCloseOrEnd(tokenIndex + 1)
		switch result := processor.tokenAt(paramsEnd + 1); {
		case result.is(token.LPAREN):
			return processor.findCloseOrEnd(paramsEnd + 1)
		case startsType(result):
			return processor.findTypeEnd(paramsEnd + 1)
		}
		return paramsEnd
	}
	if processor.tokenAt(tokenIndex+1).is(token.PERIOD) && processor.tokenAt(tokenIndex+2).is(token.IDENT) {
		tokenIndex += 2
	}
	if processor.tokenAt(tokenIndex + 1).is(token.LBRACK) {
		return processor.findCloseOrEnd(tokenIndex + 1)
	}
	return tokenIndex
}

// findCloseOrEnd returns the index of the bracket that closes the bracket at
// token openIndex, or the last token if none does.
func (processor *processor) findCloseOrEnd(openIndex int) int {
	if closeIndex := processor.findClose(openIndex); closeIndex >= 0 {
		return closeIndex
	}
	return len(processor.tokens) - 1
}

func endsOperand(current lexeme) bool {
	switch current.kind {
	case token.RPAREN, token.RBRACK, token.RBRACE:
		return true
	}
	return current.kind.IsLiteral()
}

func startsType(current lexeme) bool {
	switch current.kind {
	case token.IDENT, token.MUL, token.LBRACK, token.MAP, token.CHAN, token.ARROW, token.FUNC, token.INTERFACE, token.STRUCT:
		return true
	}
	return false
}

// isAfterOperator reports whether a unary or binary operator comes before
// token tokenIndex. The "=>" of an arrow and the ? of a ternary do not count.
func (processor *processor) isAfterOperator(tokenIndex int) bool {
	if tokenIndex == 0 || processor.ternaryParts[tokenIndex-1] {
		return false
	}
	isArrow := tokenIndex >= 2 && processor.tokens[tokenIndex-1].is(token.GTR) && processor.tokens[tokenIndex-2].is(token.ASSIGN) && processor.touchesNext(tokenIndex-2)
	return !isArrow && isOperator(processor.tokens[tokenIndex-1])
}

func isOperator(current lexeme) bool {
	switch current.kind {
	case token.ADD, token.SUB, token.MUL, token.QUO, token.REM, token.AND, token.OR, token.XOR, token.SHL, token.SHR, token.AND_NOT,
		token.LAND, token.LOR, token.ARROW, token.EQL, token.LSS, token.GTR, token.NOT, token.NEQ, token.LEQ, token.GEQ, token.TILDE:
		return true
	}
	return current.is(token.ILLEGAL) && current.text == "?"
}

// commaOk rewrites `[v, ok] = x as T` into `v, ok = x.(T)`. The pattern
// starts at open, and the = is at token operatorIndex. bindings include the
// blank elements.
func (processor *processor) commaOk(open lexeme, operatorIndex int, bindings []binding, operatorText string, isConst bool) (int, bool, error) {
	if len(bindings) != 2 || listFlatElementNames(bindings) == nil {
		return 0, false, &Error{open.offset, "a type assertion gives two values: [value, ok]"}
	}
	operatorEnd := processor.tokens[operatorIndex].end()
	// A multi-line pattern keeps its line breaks after the operator.
	newlines := strings.Repeat("\n", strings.Count(string(processor.src[open.offset:operatorEnd]), "\n"))
	assignText := names(bindings) + " " + operatorText
	namesAt := processor.replace(open.offset, operatorEnd, assignText+newlines)
	processor.recordNewlines(namesAt+len(assignText), newlines)
	if isConst {
		processor.recordConsts(namesAt, bindings)
	}
	return operatorIndex, true, nil
}

// hasTopAssertion reports whether the right side that starts at token
// tokenIndex has an as assertion outside brackets.
func (processor *processor) hasTopAssertion(tokenIndex int) bool {
	depth := 0
	for ; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch current := processor.tokens[tokenIndex]; {
		case current.is(token.LPAREN) || current.is(token.LBRACK) || current.is(token.LBRACE):
			depth++
		case current.is(token.RPAREN) || current.is(token.RBRACK) || current.is(token.RBRACE):
			depth--
			if depth < 0 {
				return false
			}
		case depth == 0 && current.is(token.SEMICOLON):
			return false
		case depth == 0 && processor.isAssertion(tokenIndex):
			return true
		}
	}
	return false
}
