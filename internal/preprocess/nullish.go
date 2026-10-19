package preprocess

import "go/token"

// OMarker marks an optional link. a?.b becomes a.__goslingO.b, a?.[i]
// becomes a.__goslingO[i], and f?.(x) becomes f.__goslingO(x).
const OMarker = "__goslingO"

// questionMark rewrites "??" (nullish coalescing), "??=" (nullish
// assignment), and "?." (optional chaining). Any other ? is an error.
func (processor *processor) questionMark(tokenIndex int) (int, error) {
	question, next := processor.tokens[tokenIndex], processor.tokenAt(tokenIndex+1)
	isAdjacent := processor.touchesNext(tokenIndex)
	switch {
	case isAdjacent && next.is(token.ILLEGAL) && next.text == "?":
		if after := processor.tokenAt(tokenIndex + 2); after.is(token.ASSIGN) && processor.touchesNext(tokenIndex+1) {
			processor.result.NullishAssigns = append(processor.result.NullishAssigns, processor.replace(question.offset, after.end(), "="))
			return tokenIndex + 2, nil
		}
		processor.result.Coalesce = append(processor.result.Coalesce, processor.replace(question.offset, next.end(), "||"))
		return tokenIndex + 1, nil
	case isAdjacent && next.is(token.PERIOD):
		if after := processor.tokenAt(tokenIndex + 2); (after.is(token.LBRACK) || after.is(token.LPAREN)) && processor.touchesNext(tokenIndex+1) {
			processor.replace(question.offset, next.end(), "."+OMarker)
			return tokenIndex + 1, nil
		}
		processor.replace(question.offset, next.end(), "."+OMarker+".")
		return tokenIndex + 1, nil
	}
	isSpacedTernary := tokenIndex > 0 && !processor.touchesNext(tokenIndex-1)
	if isSpacedTernary {
		return 0, processor.makeIncompleteTernaryError(tokenIndex)
	}
	return 0, &Error{question.offset, "the ? operator is not supported; use try/catch"}
}
