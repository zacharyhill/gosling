package preprocess

import (
	"fmt"
	"go/token"
)

// TypedMarker replaces the type of a header declaration. `let x: T = v`
// becomes `x := __goslingTyped[T](v)`.
const TypedMarker = "__goslingTyped"

// declaration rewrites TypeScript-style declarations at tokenIndex:
//
//	let x = v       ->  x := v (var x = v at package level)
//	let x: T = v    ->  var x T = v
//	let x: T        ->  var x T
//	const x: T = v  ->  const x T = v
//	let ( x: T )    ->  var ( x T ) (only at package level)
//
// A const without a type stays as written. The transpiler decides later
// whether its value is constant. In an if, for, switch, or case header,
// both let and const become :=. It returns the index of the last token it
// uses.
func (processor *processor) declaration(tokenIndex int) (int, error) {
	keyword, first := processor.tokens[tokenIndex], processor.tokenAt(tokenIndex+1)
	// A for-of rewrite already replaced this keyword.
	if keyword.offset < processor.copiedUpTo {
		return tokenIndex, nil
	}
	isHeader := processor.inHeader(tokenIndex)
	if (first.is(token.LBRACE) || first.is(token.LBRACK)) && !isHeader {
		return processor.declaredPattern(tokenIndex)
	}
	if first.is(token.LPAREN) && keyword.isIdent("let") && !isHeader && processor.isLetGroup(tokenIndex) {
		return tokenIndex, processor.rewriteGroupedLet(tokenIndex)
	}
	if !first.is(token.IDENT) {
		return tokenIndex, nil
	}
	lastNameIndex := processor.findLastName(tokenIndex + 1)
	after := processor.tokenAt(lastNameIndex + 1)
	switch {
	case after.is(token.DEFINE):
		return tokenIndex, &Error{keyword.offset, fmt.Sprintf("use %s %s = value, not :=", keyword.text, first.text)}
	case after.is(token.ASSIGN) && processor.tokenAt(lastNameIndex+2).is(token.LSS):
		return processor.rewriteGenericArrow(tokenIndex, lastNameIndex)
	case isHeader:
		return tokenIndex, processor.headerDeclaration(tokenIndex, lastNameIndex)
	case keyword.is(token.CONST):
		if after.is(token.COLON) {
			processor.scheduled[lastNameIndex+1] = ""
		}
	case after.is(token.COLON):
		processor.replace(keyword.offset, keyword.end(), "var")
		processor.scheduled[lastNameIndex+1] = ""
	case after.is(token.ASSIGN) && len(processor.braceStack) > 0:
		processor.replace(keyword.offset, first.offset, "")
		processor.scheduled[lastNameIndex+1] = ":="
	case after.is(token.ASSIGN):
		processor.replace(keyword.offset, keyword.end(), "var")
	default:
		return tokenIndex, &Error{keyword.offset, fmt.Sprintf("let %s needs a type or a value", first.text)}
	}
	return tokenIndex, nil
}

// rewriteGroupedLet rewrites `let ( a = 1; b: T )` into `var ( a = 1; b T )`. In a
// function, each let becomes :=, and try checks only these assignments. So
// a group is only allowed at package level.
func (processor *processor) rewriteGroupedLet(tokenIndex int) error {
	keyword := processor.tokens[tokenIndex]
	if len(processor.braceStack) > 0 {
		return &Error{keyword.offset, "let ( ... ) is only supported at package level; use one let for each variable"}
	}
	processor.replace(keyword.offset, keyword.end(), "var")
	for specIndex := tokenIndex + 2; specIndex < len(processor.tokens) && !processor.tokens[specIndex].is(token.RPAREN); {
		lastNameIndex, err := processor.rewriteGroupSpec(specIndex)
		if err != nil {
			return err
		}
		specIndex = processor.findNextGroupSpec(lastNameIndex + 1)
	}
	return nil
}

// isLetGroup reports whether `let (` at tokenIndex starts a group, not a call
// to a function named let. A group is the only form at package level. In a
// function, a group starts with a name and then =, :, or :=.
func (processor *processor) isLetGroup(tokenIndex int) bool {
	if len(processor.braceStack) == 0 {
		return true
	}
	firstNameIndex := tokenIndex + 2
	if !processor.tokenAt(firstNameIndex).is(token.IDENT) {
		return false
	}
	switch processor.tokenAt(processor.findLastName(firstNameIndex) + 1).kind {
	case token.ASSIGN, token.COLON, token.DEFINE:
		return true
	}
	return false
}

// rewriteGroupSpec rewrites the spec of a let group that starts at specIndex. It
// returns the index of the last name of the spec.
func (processor *processor) rewriteGroupSpec(specIndex int) (int, error) {
	first := processor.tokens[specIndex]
	if !first.is(token.IDENT) {
		return 0, &Error{first.offset, "expected a name in let ( ... )"}
	}
	lastNameIndex := processor.findLastName(specIndex)
	switch after := processor.tokenAt(lastNameIndex + 1); {
	case after.is(token.DEFINE):
		return 0, &Error{first.offset, fmt.Sprintf("use let %s = value, not :=", first.text)}
	case after.is(token.COLON):
		processor.scheduled[lastNameIndex+1] = ""
	case !after.is(token.ASSIGN):
		return 0, &Error{first.offset, fmt.Sprintf("let %s needs a type or a value", first.text)}
	}
	return lastNameIndex, nil
}

// findLastName returns the index of the last name in the list `a, b, c`
// that starts at the name at firstNameIndex.
func (processor *processor) findLastName(firstNameIndex int) int {
	lastNameIndex := firstNameIndex
	for processor.tokenAt(lastNameIndex+1).is(token.COMMA) && processor.tokenAt(lastNameIndex+2).is(token.IDENT) {
		lastNameIndex += 2
	}
	return lastNameIndex
}

// findNextGroupSpec returns the index of the token after the ; that ends the
// spec, or of the ) that ends the group.
func (processor *processor) findNextGroupSpec(from int) int {
	depth := 0
	for tokenIndex := from; tokenIndex < len(processor.tokens); tokenIndex++ {
		switch current := processor.tokens[tokenIndex]; {
		case current.is(token.LPAREN) || current.is(token.LBRACK) || current.is(token.LBRACE):
			depth++
		case current.is(token.RPAREN) || current.is(token.RBRACK) || current.is(token.RBRACE):
			if depth == 0 {
				return tokenIndex
			}
			depth--
		case current.is(token.SEMICOLON) && depth == 0:
			return tokenIndex + 1
		}
	}
	return len(processor.tokens)
}

// inHeader reports whether the token at tokenIndex starts the init statement of an if,
// for, or switch, the receive of a select case, or a type switch guard after an init.
func (processor *processor) inHeader(tokenIndex int) bool {
	if tokenIndex == 0 {
		return false
	}
	switch previous := processor.tokens[tokenIndex-1]; previous.kind {
	case token.IF, token.FOR, token.SWITCH, token.CASE:
		return true
	case token.LPAREN:
		return tokenIndex >= 2 && processor.tokens[tokenIndex-2].is(token.FOR)
	case token.SEMICOLON:
		return previous.text == ";" && processor.isSwitchInitEnd(tokenIndex-1)
	}
	return false
}

// isSwitchInitEnd reports whether the explicit semicolon at semicolonIndex
// ends the init statement of a switch.
func (processor *processor) isSwitchInitEnd(semicolonIndex int) bool {
	depth := 0
	for tokenIndex := semicolonIndex - 1; tokenIndex >= 0; tokenIndex-- {
		switch current := processor.tokens[tokenIndex]; current.kind {
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth++
		case token.LPAREN, token.LBRACK, token.LBRACE:
			if depth == 0 {
				return false
			}
			depth--
		case token.SEMICOLON:
			if depth == 0 {
				return false
			}
		case token.SWITCH:
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

// headerDeclaration rewrites `let x = v` or `const x = v` in a header into
// `x := v`, and `let x: T = v` into `x := __goslingTyped[T](v)`. Go headers
// cannot declare a type.
func (processor *processor) headerDeclaration(tokenIndex, lastNameIndex int) error {
	keyword, first, after := processor.tokens[tokenIndex], processor.tokens[tokenIndex+1], processor.tokenAt(lastNameIndex+1)
	assignIndex := lastNameIndex + 1
	if after.is(token.COLON) {
		var err error
		if assignIndex, err = processor.checkHeaderType(tokenIndex, lastNameIndex); err != nil {
			return err
		}
	}
	if !processor.tokenAt(assignIndex).is(token.ASSIGN) {
		return &Error{keyword.offset, fmt.Sprintf("%s %s needs a value", keyword.text, first.text)}
	}
	namesAt := processor.replace(keyword.offset, first.offset, "")
	if after.is(token.COLON) {
		processor.scheduled[lastNameIndex+1] = " := " + TypedMarker + "["
		processor.scheduled[assignIndex] = "]("
		// The value ends like an arrow body, after the arrows inside it.
		processor.pendingClosers = append(processor.pendingClosers, pendingCloser{start: processor.nesting(), text: ")", afterIndex: assignIndex})
	} else {
		processor.scheduled[assignIndex] = ":="
	}
	if keyword.is(token.CONST) {
		for nameIndex := tokenIndex + 1; nameIndex <= lastNameIndex; nameIndex += 2 {
			processor.result.Consts = append(processor.result.Consts, namesAt+processor.tokens[nameIndex].offset-first.offset)
		}
	}
	return nil
}

// checkHeaderType checks the type of a typed header declaration. It returns
// the index of the = after the type, or of the token that ends the declaration.
func (processor *processor) checkHeaderType(tokenIndex, lastNameIndex int) (int, error) {
	keyword := processor.tokens[tokenIndex]
	if processor.tokens[tokenIndex-1].is(token.CASE) {
		return 0, &Error{keyword.offset, fmt.Sprintf("%s in a case header cannot have a type", keyword.text)}
	}
	if lastNameIndex != tokenIndex+1 {
		return 0, &Error{keyword.offset, fmt.Sprintf("%s with a type in a header can declare only one name", keyword.text)}
	}
	assignIndex := processor.findOutsideBrackets(lastNameIndex+2, func(current lexeme) bool {
		return current.is(token.ASSIGN) || current.is(token.SEMICOLON)
	})
	if processor.tokenAt(assignIndex).is(token.ASSIGN) && processor.tokenAt(assignIndex+1).is(token.RANGE) {
		return 0, &Error{keyword.offset, fmt.Sprintf("%s with range cannot have a type", keyword.text)}
	}
	return assignIndex, nil
}

// findOutsideBrackets returns the index of the first token from start that
// matches stop and is outside brackets. It stops at an unmatched closing
// bracket or at the end of the tokens.
func (processor *processor) findOutsideBrackets(start int, stop func(lexeme) bool) int {
	depth := 0
	for tokenIndex := start; tokenIndex < len(processor.tokens); tokenIndex++ {
		current := processor.tokens[tokenIndex]
		switch {
		case current.is(token.LPAREN) || current.is(token.LBRACK) || current.is(token.LBRACE):
			depth++
		case current.is(token.RPAREN) || current.is(token.RBRACK) || current.is(token.RBRACE):
			if depth == 0 {
				return tokenIndex
			}
			depth--
		case depth == 0 && stop(current):
			return tokenIndex
		}
	}
	return len(processor.tokens)
}

// declaredPattern rewrites `let {a, b} = x` and `const [a, b] = x` into a
// destructuring with :=.
func (processor *processor) declaredPattern(tokenIndex int) (int, error) {
	keyword, open := processor.tokens[tokenIndex], processor.tokens[tokenIndex+1]
	if len(processor.braceStack) == 0 {
		return tokenIndex, &Error{keyword.offset, "destructuring with let or const is only supported in functions"}
	}
	processor.replace(keyword.offset, open.offset, "")
	next, ok, err := processor.destructure(tokenIndex+1, &keyword)
	if err == nil && !ok {
		err = &Error{open.offset, "expected a destructuring pattern"}
	}
	return next, err
}
