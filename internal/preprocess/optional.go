package preprocess

import "go/token"

// isOptionalField reports whether the ? at token tokenIndex follows a field
// name in a struct type.
func (processor *processor) isOptionalField(tokenIndex int) bool {
	stackSize := len(processor.braceStack)
	if stackSize == 0 || processor.braceStack[stackSize-1].kind != frameStruct || !processor.tokens[tokenIndex-1].is(token.IDENT) {
		return false
	}
	switch processor.tokens[tokenIndex-2].kind {
	case token.SEMICOLON, token.LBRACE, token.COMMA:
		return true
	}
	return false
}

// optionalField rewrites the field `Host? string` into `Host *string`. The
// ? is at token tokenIndex.
func (processor *processor) optionalField(tokenIndex int) (int, error) {
	question, name, typeStart := processor.tokens[tokenIndex], processor.tokens[tokenIndex-1], processor.tokenAt(tokenIndex+1)
	switch {
	case processor.tokens[tokenIndex-2].is(token.COMMA):
		return 0, &Error{question.offset, "declare optional field " + name.text + " on its own line"}
	case typeStart.is(token.COLON):
		return 0, &Error{question.offset, "write " + name.text + "? type, not " + name.text + "?: type"}
	case typeStart.is(token.MUL):
		return 0, &Error{question.offset, "optional field " + name.text + " already has a pointer type"}
	case typeStart.is(token.SEMICOLON) || typeStart.is(token.RBRACE) || typeStart.is(token.STRING):
		return 0, &Error{question.offset, "optional field " + name.text + " needs a type"}
	}
	start := processor.replace(question.offset, typeStart.offset, " *")
	processor.result.Optionals = append(processor.result.Optionals, start-(question.offset-name.offset))
	return tokenIndex, nil
}
