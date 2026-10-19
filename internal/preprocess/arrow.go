package preprocess

import (
	"bytes"
	"go/token"
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
		processor.replace(previous.offset, previous.offset, "func(")
		processor.replace(previous.end(), previous.end(), " "+UntypedMarker+")")
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
// start of a block that returns the body. finishClosers closes the block.
func (processor *processor) openExpressionBody(assign, greater, bodyStart lexeme) {
	isParenthesized := bytes.IndexByte(processor.src[greater.end():bodyStart.offset], '\n') >= 0
	returnText := " { return"
	switch {
	case isParenthesized:
		returnText += " ("
	case bodyStart.offset == greater.end():
		// Without a space, "return" joins the first token of the body.
		returnText += " "
	}
	processor.replace(assign.offset, greater.end(), ResultMarker+returnText)
	closer := " }"
	if isParenthesized {
		closer = ") }"
	}
	processor.pendingClosers = append(processor.pendingClosers, pendingCloser{start: processor.nesting(), text: closer})
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
	names, untyped := processor.untypedParams(lparenIndex+1, rparenIndex)
	lparenOffset := processor.tokens[lparenIndex].offset
	processor.replace(lparenOffset, lparenOffset, "func")
	if !untyped {
		return nil
	}
	// The names stay source text, so they map to the output.
	for _, name := range names {
		processor.replace(name.end(), name.end(), " "+UntypedMarker)
	}
	return nil
}

// untypedParams reports whether tokens [from, to) are only names, like
// "a, b", with no types. It also returns the names.
func (processor *processor) untypedParams(from, to int) ([]lexeme, bool) {
	if from == to {
		return nil, false // () needs no placeholder types
	}
	var names []lexeme
	for tokenIndex := from; tokenIndex < to; tokenIndex++ {
		current := processor.tokens[tokenIndex]
		if (tokenIndex-from)%2 == 0 {
			if !current.is(token.IDENT) {
				return nil, false
			}
			names = append(names, current)
		} else if !current.is(token.COMMA) {
			return nil, false
		}
	}
	return names, true
}
