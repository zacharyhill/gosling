package preprocess

import "go/token"

// using rewrites `using x = v` at token tokenIndex into
// `x := v; defer x.Close()`. The transpiler checks the error of v. It
// returns the index of the last token it uses.
func (processor *processor) using(tokenIndex int) (int, error) {
	keyword, name := processor.tokens[tokenIndex], processor.tokens[tokenIndex+1]
	if len(processor.braceStack) == 0 {
		return 0, &Error{keyword.offset, "using is only supported in functions"}
	}
	nameAt := processor.replace(keyword.offset, name.offset, "")
	processor.result.Consts = append(processor.result.Consts, nameAt)
	processor.result.Usings = append(processor.result.Usings, nameAt)
	processor.scheduled[tokenIndex+2] = ":="
	// The defer goes after the value, as the bindings of a destructuring do.
	processor.pendingDestructures = append(processor.pendingDestructures, &destructure{
		start: processor.nesting(),
		text:  "; defer " + name.text + ".Close()",
	})
	return tokenIndex + 1, nil
}

// isUsing reports whether the tokens at tokenIndex are `using name =`.
func (processor *processor) isUsing(tokenIndex int) bool {
	return processor.tokens[tokenIndex].isIdent("using") && processor.atStmtStart(tokenIndex) &&
		processor.tokenAt(tokenIndex+1).is(token.IDENT) && processor.tokenAt(tokenIndex+2).is(token.ASSIGN)
}
