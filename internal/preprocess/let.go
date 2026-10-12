package preprocess

import (
	"fmt"
	"go/token"
)

// declaration rewrites TypeScript-style declarations at tokenIndex:
//
//	let x = v       ->  x := v (var x = v at package level)
//	let x: T = v    ->  var x T = v
//	let x: T        ->  var x T
//	const x: T = v  ->  const x T = v
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
	header := processor.inHeader(tokenIndex)
	if (first.is(token.LBRACE) || first.is(token.LBRACK)) && !header {
		return processor.declaredPattern(tokenIndex)
	}
	if !first.is(token.IDENT) {
		return tokenIndex, nil
	}
	lastName := tokenIndex + 1
	for processor.tokenAt(lastName+1).is(token.COMMA) && processor.tokenAt(lastName+2).is(token.IDENT) {
		lastName += 2
	}
	after := processor.tokenAt(lastName + 1)
	switch {
	case after.is(token.DEFINE):
		return tokenIndex, &Error{keyword.offset, fmt.Sprintf("use %s %s = value, not :=", keyword.text, first.text)}
	case header:
		return tokenIndex, processor.headerDeclaration(tokenIndex, lastName)
	case keyword.is(token.CONST):
		if after.is(token.COLON) {
			processor.scheduled[lastName+1] = ""
		}
	case after.is(token.COLON):
		processor.replace(keyword.offset, keyword.end(), "var")
		processor.scheduled[lastName+1] = ""
	case after.is(token.ASSIGN) && len(processor.braceStack) > 0:
		processor.replace(keyword.offset, first.offset, "")
		processor.scheduled[lastName+1] = ":="
	case after.is(token.ASSIGN):
		processor.replace(keyword.offset, keyword.end(), "var")
	default:
		return tokenIndex, &Error{keyword.offset, fmt.Sprintf("let %s needs a type or a value", first.text)}
	}
	return tokenIndex, nil
}

// inHeader reports whether the token at tokenIndex starts the init
// statement of an if, for, or switch, or the receive of a select case.
func (processor *processor) inHeader(tokenIndex int) bool {
	if tokenIndex == 0 {
		return false
	}
	switch previous := processor.tokens[tokenIndex-1]; previous.kind {
	case token.IF, token.FOR, token.SWITCH, token.CASE:
		return true
	case token.LPAREN:
		return tokenIndex >= 2 && processor.tokens[tokenIndex-2].is(token.FOR)
	}
	return false
}

// headerDeclaration rewrites `let x = v` or `const x = v` in a header into
// `x := v`. Go headers cannot declare a type.
func (processor *processor) headerDeclaration(tokenIndex, lastName int) error {
	keyword, first, after := processor.tokens[tokenIndex], processor.tokens[tokenIndex+1], processor.tokenAt(lastName+1)
	if after.is(token.COLON) {
		return &Error{keyword.offset, fmt.Sprintf("%s in an if, for, switch, or case header cannot have a type", keyword.text)}
	}
	if !after.is(token.ASSIGN) {
		return &Error{keyword.offset, fmt.Sprintf("%s %s needs a value", keyword.text, first.text)}
	}
	namesAt := processor.replace(keyword.offset, first.offset, "")
	processor.scheduled[lastName+1] = ":="
	if keyword.is(token.CONST) {
		for nameIndex := tokenIndex + 1; nameIndex <= lastName; nameIndex += 2 {
			processor.result.Consts = append(processor.result.Consts, namesAt+processor.tokens[nameIndex].offset-first.offset)
		}
	}
	return nil
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
