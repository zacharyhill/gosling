package preprocess

import (
	"bytes"
	"go/token"
)

// Arrow functions become function literals with placeholder types. The
// transpiler replaces them with the types that type checking finds:
//
//	x => x * 2                func(x __goslingU) __goslingR { return x * 2 }
//	(x int) => { ... }        func(x int) __goslingR { ... }
//	(x): string => f(x)       func(x __goslingU) string { return f(x) }
//	(x: int): string => f(x)  func(x int) string { return f(x) }
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
	if closeOffset, isResultArrow := processor.resultArrows[assign.offset]; isResultArrow {
		resultEnd := processor.tokens[tokenIndex-1].end()
		return processor.openArrowBody(tokenIndex, resultEnd, "", processor.arrowStatements[closeOffset]), nil
	}
	previous := processor.tokens[tokenIndex-1]
	var statements paramStatements
	switch {
	case previous.is(token.IDENT):
		if previous.offset < processor.copiedUpTo {
			return 0, &Error{previous.offset, "unsupported arrow function"}
		}
		processor.replace(previous.offset, previous.offset, "func(")
		processor.replace(previous.end(), previous.end(), " "+UntypedMarker+")")
	case previous.is(token.RPAREN):
		var err error
		if statements, err = processor.arrowParams(tokenIndex - 1); err != nil {
			return 0, err
		}
	default:
		return 0, &Error{assign.offset, "=> must follow a parameter list"}
	}
	return processor.openArrowBody(tokenIndex, assign.offset, ResultMarker, statements), nil
}

// openArrowBody replaces the source from start to the end of the "=>" at token
// tokenIndex with result, and opens the body. statements declare the names
// of destructured parameters. It returns the index of the last token it uses.
func (processor *processor) openArrowBody(tokenIndex, start int, result string, statements paramStatements) int {
	greater := processor.tokenAt(tokenIndex + 1)
	if processor.tokenAt(tokenIndex + 2).is(token.LBRACE) {
		processor.replace(start, greater.end(), result)
		if statements.text != "" {
			processor.bodyStatements[tokenIndex+2] = statements
		}
		return tokenIndex + 1
	}
	processor.openExpressionBody(start, greater, processor.tokenAt(tokenIndex+2), result, statements)
	return tokenIndex + 1
}

// openExpressionBody replaces the source from start to the end of the "=>"
// of an expression-body arrow with result and the start of a block that
// returns the body. finishClosers closes the block.
func (processor *processor) openExpressionBody(start int, greater, bodyStart lexeme, result string, statements paramStatements) {
	isParenthesized := bytes.IndexByte(processor.src[greater.end():bodyStart.offset], '\n') >= 0
	returnText := " return"
	switch {
	case isParenthesized:
		returnText += " ("
	case bodyStart.offset == greater.end():
		// Without a space, "return" joins the first token of the body.
		returnText += " "
	}
	if statements.text == "" {
		processor.replace(start, greater.end(), result+" {"+returnText)
	} else {
		processor.replace(start, greater.end(), result+" {")
		processor.insertParamStatements(greater.end(), statements)
		processor.replace(greater.end(), greater.end(), returnText)
	}
	closer := " }"
	if isParenthesized {
		closer = ") }"
	}
	processor.pendingClosers = append(processor.pendingClosers, pendingCloser{start: processor.nesting(), text: closer})
}

// arrowParams rewrites the parameter list closed by the ) at token
// rparenIndex. It returns the statements for destructured parameters.
func (processor *processor) arrowParams(rparenIndex int) (paramStatements, error) {
	if rparenOffset := processor.tokens[rparenIndex].offset; processor.typedArrows[rparenOffset] {
		return processor.arrowStatements[rparenOffset], nil
	}
	lparenIndex := processor.findMatchingLParen(rparenIndex)
	if lparenIndex < 0 || processor.tokens[lparenIndex].offset < processor.copiedUpTo {
		return paramStatements{}, &Error{processor.tokens[rparenIndex].offset, "unsupported arrow function"}
	}
	names, untyped := processor.untypedParams(lparenIndex+1, rparenIndex)
	lparenOffset := processor.tokens[lparenIndex].offset
	processor.replace(lparenOffset, lparenOffset, "func")
	if !untyped {
		return paramStatements{}, nil
	}
	// The names stay source text, so they map to the output.
	for _, name := range names {
		processor.replace(name.end(), name.end(), " "+UntypedMarker)
	}
	return paramStatements{}, nil
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

// rewriteGenericArrow rewrites a package-level const whose value is a generic
// arrow into a generic function declaration:
//
//	const id = <T>(x T) => x    func id[T any](x T) __goslingR { return x }
//
// If the last type parameter has no constraint, it gets any. So <K, V>
// becomes [K, V any], as in Go. It returns the index of the last token it
// uses.
func (processor *processor) rewriteGenericArrow(tokenIndex, lastNameIndex int) (int, error) {
	keyword, name := processor.tokens[tokenIndex], processor.tokens[tokenIndex+1]
	if !keyword.is(token.CONST) || len(processor.braceStack) > 0 || lastNameIndex != tokenIndex+1 {
		return 0, &Error{keyword.offset, "a generic arrow function must be a package-level const with one name"}
	}
	lessIndex := lastNameIndex + 2
	greaterIndex, ok := processor.findTypeParamsEnd(lessIndex)
	if !ok || !processor.tokenAt(greaterIndex+1).is(token.LPAREN) || !processor.hasArrowAfter(greaterIndex+1) {
		return 0, &Error{processor.tokens[lessIndex].offset, "expected an arrow function after the type parameters, as in <T>(x T) => x"}
	}
	less, greater := processor.tokens[lessIndex], processor.tokens[greaterIndex]
	closingText := "]"
	if previous := processor.tokens[greaterIndex-2]; previous.is(token.COMMA) || previous.is(token.LSS) {
		closingText = " any]"
	}
	processor.replace(keyword.offset, name.offset, "func ")
	processor.replace(name.end(), less.end(), "[")
	processor.replace(greater.offset, greater.end(), closingText)
	processor.genericParamOffsets[processor.tokens[greaterIndex+1].offset] = true
	return greaterIndex, nil
}

// findTypeParamsEnd returns the index of the > that closes the type parameter
// list at the < at token lessIndex. The list must not be empty.
func (processor *processor) findTypeParamsEnd(lessIndex int) (int, bool) {
	depth := 0
	for tokenIndex := lessIndex + 1; tokenIndex < len(processor.tokens); tokenIndex++ {
		current := processor.tokens[tokenIndex]
		switch {
		case current.is(token.GTR) && depth == 0:
			return tokenIndex, tokenIndex > lessIndex+1
		case isOpener(current):
			depth++
		case isCloser(current):
			depth--
		case current.is(token.SEMICOLON) && depth == 0:
			return 0, false
		}
		if depth < 0 {
			return 0, false
		}
	}
	return 0, false
}

// hasArrowAfter reports whether "=>" follows the parameter list that starts
// at the ( at token lparenIndex, maybe after a result type.
func (processor *processor) hasArrowAfter(lparenIndex int) bool {
	depth := 0
	for tokenIndex := lparenIndex; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch processor.tokens[tokenIndex].kind {
		case token.LPAREN:
			depth++
		case token.RPAREN:
			depth--
		}
		if depth == 0 {
			return processor.isArrow(tokenIndex+1) || processor.isArrowResultColon(tokenIndex+1)
		}
	}
	return false
}

func isOpener(current lexeme) bool {
	return current.is(token.LPAREN) || current.is(token.LBRACK) || current.is(token.LBRACE)
}

func isCloser(current lexeme) bool {
	return current.is(token.RPAREN) || current.is(token.RBRACK) || current.is(token.RBRACE)
}

// findArrowAfterResult returns the index of the "=>" after the result type that
// starts at the colon at token colonIndex. The colon must follow a
// parameter list. A list after an operand is a call, as in {f(a): x => x},
// and a list after func is a function type.
func (processor *processor) findArrowAfterResult(colonIndex int) (int, bool) {
	if !processor.followsParamList(colonIndex) || processor.isFuncTypeColon(colonIndex) {
		return 0, false
	}
	depth := 0
	for tokenIndex := colonIndex + 1; tokenIndex < len(processor.tokens); tokenIndex++ {
		current := processor.tokens[tokenIndex]
		switch {
		case current.is(token.ASSIGN) && depth == 0:
			return tokenIndex, tokenIndex > colonIndex+1 && processor.isArrow(tokenIndex)
		case isOpener(current):
			depth++
		case isCloser(current):
			depth--
			if depth < 0 {
				return 0, false
			}
		case current.is(token.COLON):
			// Inside brackets a colon annotates a name. Outside, it starts the
			// result of a function type, as in (): func(x: int): int => ...
			if depth == 0 && !processor.isFuncTypeColon(tokenIndex) {
				return 0, false
			}
		case !isTypeToken(current, depth):
			return 0, false
		}
	}
	return 0, false
}

// isArrowResultColon reports whether the colon at colonIndex starts the
// result type of an arrow.
func (processor *processor) isArrowResultColon(colonIndex int) bool {
	_, ok := processor.findArrowAfterResult(colonIndex)
	return ok
}

// followsParamList reports whether the token at colonIndex is a colon after
// a parenthesized list that is not a call.
func (processor *processor) followsParamList(colonIndex int) bool {
	if colonIndex == 0 || !processor.tokenAt(colonIndex).is(token.COLON) || !processor.tokens[colonIndex-1].is(token.RPAREN) {
		return false
	}
	lparenIndex := processor.findMatchingLParen(colonIndex - 1)
	return lparenIndex == 0 || (lparenIndex > 0 && !isOperandEnd(processor.tokens[lparenIndex-1]))
}

// isFuncTypeColon reports whether the colon at colonIndex follows the
// parameters of a function type, as in func(x: int): int.
func (processor *processor) isFuncTypeColon(colonIndex int) bool {
	if colonIndex == 0 || !processor.tokens[colonIndex-1].is(token.RPAREN) {
		return false
	}
	lparenIndex := processor.findMatchingLParen(colonIndex - 1)
	return lparenIndex > 0 && processor.tokens[lparenIndex-1].is(token.FUNC)
}

// isTypeToken reports whether current can be part of a type expression at
// the bracket depth. Commas, semicolons, and tags occur only in brackets.
func isTypeToken(current lexeme, depth int) bool {
	switch current.kind {
	case token.IDENT, token.PERIOD, token.MUL, token.MAP, token.CHAN, token.ARROW, token.FUNC,
		token.STRUCT, token.INTERFACE, token.INT, token.ELLIPSIS:
		return true
	case token.COMMA, token.SEMICOLON, token.STRING:
		return depth > 0
	}
	return false
}

func isOperandEnd(current lexeme) bool {
	switch current.kind {
	case token.IDENT, token.INT, token.FLOAT, token.IMAG, token.CHAR, token.STRING,
		token.RPAREN, token.RBRACK, token.RBRACE, token.INC, token.DEC:
		return true
	}
	return false
}

// findMatchingLParen returns the index of the ( that the ) at token rparenIndex
// closes, or -1.
func (processor *processor) findMatchingLParen(rparenIndex int) int {
	depth := 0
	for tokenIndex := rparenIndex; tokenIndex >= 0; tokenIndex-- {
		switch processor.tokens[tokenIndex].kind {
		case token.RPAREN:
			depth++
		case token.LPAREN:
			depth--
		}
		if depth == 0 {
			return tokenIndex
		}
	}
	return -1
}
