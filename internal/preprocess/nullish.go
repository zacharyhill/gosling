package preprocess

import "go/token"

// OMarker marks an optional selector. a?.b becomes a.__goslingO.b.
const OMarker = "__goslingO"

// questionMark rewrites "??" (nullish coalescing) and "?." (optional
// chaining). Any other ? is an error.
func (processor *processor) questionMark(tokenIndex int) (int, error) {
	question, next := processor.tokens[tokenIndex], processor.tokenAt(tokenIndex+1)
	isAdjacent := processor.touchesNext(tokenIndex)
	switch {
	case isAdjacent && next.is(token.ILLEGAL) && next.text == "?":
		if after := processor.tokenAt(tokenIndex + 2); after.is(token.ASSIGN) && processor.touchesNext(tokenIndex+1) {
			return 0, &Error{question.offset, "??= is not supported"}
		}
		processor.result.Coalesce = append(processor.result.Coalesce, processor.replace(question.offset, next.end(), "||"))
		return tokenIndex + 1, nil
	case isAdjacent && next.is(token.PERIOD):
		if after := processor.tokenAt(tokenIndex + 2); (after.is(token.LBRACK) || after.is(token.LPAREN)) && processor.touchesNext(tokenIndex+1) {
			return 0, &Error{question.offset, "?.[ and ?.( are not supported"}
		}
		processor.replace(question.offset, next.end(), "."+OMarker+".")
		return tokenIndex + 1, nil
	}
	return 0, &Error{question.offset, "the ? operator is not supported; use try/catch"}
}
