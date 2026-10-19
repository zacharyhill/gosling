package preprocess

import (
	"go/token"
	"strings"
)

// TernaryMarker is the function that replaces a ternary. c ? a : b becomes
// __goslingQ(c , a , b).
const TernaryMarker = "__goslingQ"

// findTernaries finds the ternaries in the tokens of the current scan. A ?
// without a matching : is not a ternary. questionMark reports it later.
func (processor *processor) findTernaries() {
	for tokenIndex := range processor.tokens {
		// A ternary arm cannot be empty, so ?: is an optional parameter.
		if !processor.isLoneQuestion(tokenIndex) || processor.tokenAt(tokenIndex+1).is(token.COLON) {
			continue
		}
		colonIndex, ok := processor.findTernaryColon(tokenIndex)
		if !ok {
			continue
		}
		processor.ternaryEnds[tokenIndex] = processor.findTernaryEnd(tokenIndex, colonIndex)
		processor.ternaryParts[tokenIndex] = true
		processor.ternaryParts[colonIndex] = true
		processor.ternaryStarts[processor.findConditionStart(tokenIndex)]++
	}
}

// makeIncompleteTernaryError returns the error for the ? at token
// questionIndex that has no matching :.
func (processor *processor) makeIncompleteTernaryError(questionIndex int) error {
	question, armIndex := processor.tokens[questionIndex], questionIndex+1
	if processor.isCanceledSemicolon(armIndex) {
		armIndex++
	}
	if processor.isTernaryArmEnd(armIndex) {
		return &Error{question.offset, "ternary is missing the value after '?'"}
	}
	return &Error{question.offset, "ternary is missing ':'"}
}

// isTernaryArmEnd reports whether the token at tokenIndex cannot start the
// value of a ternary arm.
func (processor *processor) isTernaryArmEnd(tokenIndex int) bool {
	switch processor.tokenAt(tokenIndex).kind {
	case token.RPAREN, token.RBRACK, token.RBRACE, token.COMMA, token.SEMICOLON, token.COLON, token.EOF:
		return true
	}
	return false
}

// isLoneQuestion reports whether the token at tokenIndex is a ? that is not
// part of ?? or ?.
func (processor *processor) isLoneQuestion(tokenIndex int) bool {
	if !isQuestion(processor.tokenAt(tokenIndex)) {
		return false
	}
	if next := processor.tokenAt(tokenIndex + 1); (isQuestion(next) || next.is(token.PERIOD)) && processor.touchesNext(tokenIndex) {
		return false
	}
	return tokenIndex == 0 || !isQuestion(processor.tokens[tokenIndex-1]) || !processor.touchesNext(tokenIndex-1)
}

func isQuestion(current lexeme) bool { return current.is(token.ILLEGAL) && current.text == "?" }

// isCanceledSemicolon reports whether the token at tokenIndex is the automatic
// semicolon after a ? at the end of a line. The ternary continues on the
// next line.
func (processor *processor) isCanceledSemicolon(tokenIndex int) bool {
	return processor.tokenAt(tokenIndex).isAutoSemicolon() && tokenIndex > 0 && processor.isLoneQuestion(tokenIndex-1)
}

// findTernaryColon finds the : of the ternary whose ? is at token questionIndex.
// A nested ternary in the then arm has its own :.
func (processor *processor) findTernaryColon(questionIndex int) (int, bool) {
	depth, nested := 0, 0
	for tokenIndex := questionIndex + 1; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch current := processor.tokens[tokenIndex]; current.kind {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			if depth == 0 {
				return 0, false
			}
			depth--
		case token.COMMA, token.SEMICOLON:
			if depth == 0 && !processor.isCanceledSemicolon(tokenIndex) {
				return 0, false
			}
		case token.COLON:
			if depth > 0 {
				continue
			}
			if nested == 0 {
				return tokenIndex, true
			}
			nested--
		case token.ILLEGAL:
			if depth == 0 && processor.isLoneQuestion(tokenIndex) {
				nested++
			}
		}
	}
	return 0, false
}

// findTernaryEnd returns the index of the token after the else arm that starts
// after token colonIndex. A nested ternary in the else arm belongs to it.
func (processor *processor) findTernaryEnd(questionIndex, colonIndex int) int {
	isHeader := processor.isHeaderExpr(questionIndex)
	depth, nested := 0, 0
	for tokenIndex := colonIndex + 1; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch current := processor.tokens[tokenIndex]; current.kind {
		case token.LBRACE:
			// In a header, { at depth 0 opens the block, as in Go.
			if depth == 0 && isHeader {
				return tokenIndex
			}
			depth++
		case token.LPAREN, token.LBRACK:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			if depth == 0 {
				return tokenIndex
			}
			depth--
		case token.COMMA, token.SEMICOLON:
			if depth == 0 && !processor.isCanceledSemicolon(tokenIndex) {
				return tokenIndex
			}
		case token.COLON:
			if depth > 0 {
				continue
			}
			if nested == 0 {
				return tokenIndex
			}
			nested--
		case token.ILLEGAL:
			if depth == 0 && processor.isLoneQuestion(tokenIndex) {
				nested++
			}
		}
	}
	return len(processor.tokens)
}

// findConditionStart returns the index of the first token of the condition
// before the ? at token questionIndex. A ternary has the lowest precedence,
// so the condition reaches back to the start of the expression.
func (processor *processor) findConditionStart(questionIndex int) int {
	depth := 0
	for tokenIndex := questionIndex - 1; tokenIndex >= 0; tokenIndex-- {
		current := processor.tokens[tokenIndex]
		switch current.kind {
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth++
			continue
		case token.LPAREN, token.LBRACK, token.LBRACE:
			if depth == 0 {
				return tokenIndex + 1
			}
			depth--
			continue
		}
		if depth == 0 && processor.isBeforeCondition(tokenIndex) {
			return tokenIndex + 1
		}
	}
	return 0
}

// isBeforeCondition reports whether the token at tokenIndex comes directly
// before the condition of a ternary.
func (processor *processor) isBeforeCondition(tokenIndex int) bool {
	current := processor.tokens[tokenIndex]
	switch current.kind {
	case token.SEMICOLON:
		return !processor.isCanceledSemicolon(tokenIndex)
	case token.COMMA, token.COLON, token.ASSIGN, token.DEFINE,
		token.RETURN, token.CASE, token.IF, token.SWITCH, token.FOR, token.RANGE, token.GO, token.DEFER:
		return true
	case token.GTR:
		// The > of =>.
		return tokenIndex > 0 && processor.tokens[tokenIndex-1].is(token.ASSIGN) && processor.touchesNext(tokenIndex-1)
	case token.ARROW:
		// A send, not a receive.
		return tokenIndex > 0 && isOperandEnd(processor.tokens[tokenIndex-1])
	case token.ILLEGAL:
		return processor.isLoneQuestion(tokenIndex)
	case token.IDENT:
		return processor.isYieldKeyword(tokenIndex)
	case token.MUL:
		// The * of yield*.
		return tokenIndex > 0 && processor.isYieldKeyword(tokenIndex-1) && processor.touchesNext(tokenIndex-1)
	}
	return current.kind >= token.ADD_ASSIGN && current.kind <= token.AND_NOT_ASSIGN
}

// isYieldKeyword reports whether the token at tokenIndex can start a yield
// statement. A use of a variable named yield, as in yield := 1, cannot.
func (processor *processor) isYieldKeyword(tokenIndex int) bool {
	if !processor.tokens[tokenIndex].isIdent("yield") || !processor.atStmtStart(tokenIndex) {
		return false
	}
	switch next := processor.tokenAt(tokenIndex + 1); next.kind {
	case token.DEFINE, token.ASSIGN, token.PERIOD, token.COMMA, token.COLON, token.INC, token.DEC, token.SEMICOLON, token.RBRACE, token.EOF:
		return false
	default:
		return next.kind < token.ADD_ASSIGN || next.kind > token.AND_NOT_ASSIGN
	}
}

// isHeaderExpr reports whether the ? at token questionIndex is at the top
// level of an if, for, or switch header.
func (processor *processor) isHeaderExpr(questionIndex int) bool {
	depth := 0
	for tokenIndex := questionIndex - 1; tokenIndex >= 0; tokenIndex-- {
		switch current := processor.tokens[tokenIndex]; current.kind {
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth++
		case token.LPAREN, token.LBRACK, token.LBRACE:
			if depth == 0 {
				return false
			}
			depth--
		case token.SEMICOLON:
			if depth == 0 && current.isAutoSemicolon() && !processor.isCanceledSemicolon(tokenIndex) {
				return false
			}
		case token.IF, token.FOR, token.SWITCH:
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

// openTernaries writes the markers for the ternaries whose condition starts
// at token tokenIndex.
func (processor *processor) openTernaries(tokenIndex int) {
	count := processor.ternaryStarts[tokenIndex]
	if count == 0 {
		return
	}
	offset := processor.tokens[tokenIndex].offset
	processor.replace(offset, offset, strings.Repeat(TernaryMarker+"(", count))
}

// rewriteTernary rewrites the ? at token tokenIndex and schedules the rest
// of the ternary.
func (processor *processor) rewriteTernary(tokenIndex int) {
	question := processor.tokens[tokenIndex]
	processor.replace(question.offset, question.end(), ",")
	processor.pendingClosers = append(processor.pendingClosers, pendingCloser{ternaryEnd: processor.ternaryEnds[tokenIndex]})
}

func (processor *processor) resetTernaries() {
	processor.ternaryEnds, processor.ternaryParts, processor.ternaryStarts = map[int]int{}, map[int]bool{}, map[int]int{}
}
