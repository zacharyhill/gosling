package preprocess

import (
	"bytes"
	"go/token"
	"strings"
)

// Arrow functions become function literals with placeholder types. The
// transpiler replaces them with the types that type checking finds:
//
//	x => x * 2         func(x __goslingU) __goslingR { return x * 2 }
//	(x int) => { ... } func(x int) __goslingR { ... }
const (
	UntypedMarker = "__goslingU"
	ResultMarker  = "__goslingR"
)

// pendingArrow is an arrow with an expression body that the scan has not
// finished.
type pendingArrow struct {
	start nesting
	// isParenthesized is true when the body starts on a later line. A newline
	// directly after return ends the return statement.
	isParenthesized bool
}

// arrow rewrites the arrow function whose "=>" starts at token tokenIndex.
func (processor *processor) arrow(tokenIndex int) (int, error) {
	assign := processor.tokens[tokenIndex]
	if tokenIndex == 0 {
		return 0, &Error{assign.offset, "=> must follow a parameter list"}
	}
	previous := processor.tokens[tokenIndex-1]
	switch {
	case previous.is(token.IDENT):
		if previous.offset < processor.copiedUpTo {
			return 0, &Error{previous.offset, "unsupported arrow function"}
		}
		processor.replace(previous.offset, previous.end(), "func("+previous.text+" "+UntypedMarker+")")
	case previous.is(token.RPAREN):
		if err := processor.arrowParams(tokenIndex - 1); err != nil {
			return 0, err
		}
	default:
		return 0, &Error{assign.offset, "=> must follow a parameter list"}
	}
	greater := processor.tokenAt(tokenIndex + 1)
	if processor.tokenAt(tokenIndex + 2).is(token.LBRACE) {
		processor.replace(assign.offset, greater.end(), ResultMarker)
		return tokenIndex + 1, nil
	}
	processor.openExpressionBody(assign, greater, processor.tokenAt(tokenIndex+2))
	return tokenIndex + 1, nil
}

// openExpressionBody replaces the "=>" of an expression-body arrow with the
// start of a block that returns the body. finishArrows closes the block.
func (processor *processor) openExpressionBody(assign, greater, bodyStart lexeme) {
	isParenthesized := bytes.IndexByte(processor.src[greater.end():bodyStart.offset], '\n') >= 0
	returnText := " { return"
	if isParenthesized {
		returnText += " ("
	}
	processor.replace(assign.offset, greater.end(), ResultMarker+returnText)
	processor.pendingArrows = append(processor.pendingArrows, pendingArrow{start: processor.nesting(), isParenthesized: isParenthesized})
}

// arrowParams rewrites the parameter list closed by the ) at token rparenIndex.
func (processor *processor) arrowParams(rparenIndex int) error {
	if processor.typedArrows[processor.tokens[rparenIndex].offset] {
		return nil
	}
	lparenIndex, depth := -1, 0
	for tokenIndex := rparenIndex; tokenIndex >= 0; tokenIndex-- {
		switch processor.tokens[tokenIndex].kind {
		case token.RPAREN:
			depth++
		case token.LPAREN:
			depth--
		}
		if depth == 0 {
			lparenIndex = tokenIndex
			break
		}
	}
	if lparenIndex < 0 || processor.tokens[lparenIndex].offset < processor.copiedUpTo {
		return &Error{processor.tokens[rparenIndex].offset, "unsupported arrow function"}
	}
	lparen, rparen := processor.tokens[lparenIndex], processor.tokens[rparenIndex]
	names, untyped := processor.untypedParams(lparenIndex+1, rparenIndex)
	if !untyped {
		processor.replace(lparen.offset, lparen.offset, "func")
		return nil
	}
	for nameIndex, name := range names {
		names[nameIndex] = name + " " + UntypedMarker
	}
	processor.replace(lparen.offset, rparen.end(), "func("+strings.Join(names, ", ")+")")
	return nil
}

// untypedParams reports whether tokens [from, to) are only names, like
// "a, b", with no types. It also returns the names.
func (processor *processor) untypedParams(from, to int) ([]string, bool) {
	if from == to {
		return nil, false // () needs no placeholder types
	}
	var names []string
	for tokenIndex := from; tokenIndex < to; tokenIndex++ {
		current := processor.tokens[tokenIndex]
		if (tokenIndex-from)%2 == 0 {
			if !current.is(token.IDENT) {
				return nil, false
			}
			names = append(names, current.text)
		} else if !current.is(token.COMMA) {
			return nil, false
		}
	}
	return names, true
}

// finishArrows closes the pending arrows whose body ends at token tokenIndex.
func (processor *processor) finishArrows(tokenIndex int) {
	for len(processor.pendingArrows) > 0 {
		pending := processor.pendingArrows[len(processor.pendingArrows)-1]
		if processor.nesting() != pending.start {
			return
		}
		switch processor.tokenAt(tokenIndex).kind {
		case token.COMMA, token.RPAREN, token.RBRACK, token.RBRACE, token.SEMICOLON, token.COLON, token.EOF:
		default:
			return
		}
		processor.pendingArrows = processor.pendingArrows[:len(processor.pendingArrows)-1]
		end := processor.tokenAt(tokenIndex - 1).end()
		if pending.isParenthesized {
			processor.replace(end, end, ") }")
		} else {
			processor.replace(end, end, " }")
		}
	}
}
